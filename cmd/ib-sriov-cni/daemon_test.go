package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/guidcache"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/podcache"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

const testNodeName = "node-a"

func cachedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1", Name: "vm-1", UID: "uid-1", ResourceVersion: "1",
			Annotations: map[string]string{testNetworksAnnotation: `[{"name":"net-a","interface":"ib1"}]`},
		},
		Spec: corev1.PodSpec{NodeName: testNodeName},
	}
}

// startNodeCache returns a synchronized cache plus the clientset behind it, so
// a test can count exactly what the API server was asked for.
func startNodeCache(t *testing.T, objects ...runtime.Object) (*podcache.Cache, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(objects...)
	watching := make(chan struct{})
	var once sync.Once
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		once.Do(func() { close(watching) })
		return false, nil, nil
	})
	pods, err := podcache.New(client, testNodeName)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pods.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("pod cache did not shut down")
		}
	})
	syncCtx, stopSync := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopSync()
	if !pods.WaitForSync(syncCtx) {
		t.Fatal("pod cache did not sync")
	}
	select {
	case <-watching:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not start")
	}
	return pods, client
}

func TestDaemonServesCachedPodsWithoutReadingTheAPIServer(t *testing.T) {
	pods, client := startNodeCache(t, cachedPod())
	api, _ := newAnnotationTest(t)
	api.getErr = errors.New("the daemon must not read the API server for a cached Pod")
	reader := nodePodReader{pods: pods, api: api}
	for range 100 {
		pod, err := reader.GetPod(context.Background(), "ns1", "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		if pod.Name != "vm-1" {
			t.Fatalf("read %q, want vm-1", pod.Name)
		}
	}
	if api.getCalls != 0 {
		t.Fatalf("live reads = %d, want 0", api.getCalls)
	}
	// One LIST and one WATCH establish the cache; nothing else is charged to
	// the API server no matter how many attachments the node starts.
	if actions := client.Actions(); len(actions) != 2 {
		t.Fatalf("API actions = %v, want one LIST and one WATCH", actions)
	}
}

func TestDaemonFallsBackToALiveReadForAnUncachedPod(t *testing.T) {
	pods, _ := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	reader := nodePodReader{pods: pods, api: api}
	pod, err := reader.GetPod(context.Background(), "ns1", "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Name != "vm-1" || api.getCalls != 1 {
		t.Fatalf("read %q after %d live reads, want vm-1 after 1", pod.Name, api.getCalls)
	}
}

func TestDaemonSurfacesACacheThatIsNotReady(t *testing.T) {
	pods, err := podcache.New(fake.NewClientset(), testNodeName)
	if err != nil {
		t.Fatal(err)
	}
	api, _ := newAnnotationTest(t)
	reader := nodePodReader{pods: pods, api: api}
	// An unsynchronized cache reports neither the Pod nor its absence, so it
	// must not be mistaken for a miss and answered from the API server.
	if _, err = reader.GetPod(context.Background(), "ns1", "vm-1"); !errors.Is(err, podcache.ErrNotReady) {
		t.Fatalf("read from an unsynchronized cache = %v, want ErrNotReady", err)
	}
	if api.getCalls != 0 {
		t.Fatalf("live reads = %d, want 0", api.getCalls)
	}
}

func TestDaemonReadsLiveAfterARejectedWrite(t *testing.T) {
	pods, _ := startNodeCache(t, cachedPod())
	api, _ := newAnnotationTest(t)
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}
	if _, err := writer.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 0 {
		t.Fatalf("live reads before a write = %d, want 0", api.getCalls)
	}
	api.patchHook = func() error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("stale"))
	}
	if err := writer.PatchAnnotation(context.Background(), "ns1", "vm-1", "{}", "1", true); err == nil {
		t.Fatal("rejected write reported success")
	}
	// Re-reading the same cached copy could only reproduce the rejection.
	if _, err := writer.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 1 {
		t.Fatalf("live reads after a rejected write = %d, want 1", api.getCalls)
	}
}

func TestDaemonKeepsReadingTheCacheWhileWritesSucceed(t *testing.T) {
	pods, _ := startNodeCache(t, cachedPod())
	api, _ := newAnnotationTest(t)
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}
	if err := writer.PatchAnnotation(context.Background(), "ns1", "vm-1", "{}", "1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 0 {
		t.Fatalf("live reads after an accepted write = %d, want 0", api.getCalls)
	}
}

func TestDelegateConfigurationsAreRefusedWhereTheyCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delegates bool
		ipam      string
		vfio      bool
		wantErr   bool
	}{
		{name: "standalone runs IPAM", delegates: true, ipam: "host-local"},
		{name: "daemon refuses IPAM", ipam: "host-local", wantErr: true},
		{name: "daemon allows no IPAM", ipam: ""},
		{name: "daemon allows unused IPAM on vfio", ipam: "host-local", vfio: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &localtypes.NetConf{}
			conf.IPAM.Type, conf.VfioPciMode = tc.ipam, tc.vfio
			err := invocation{delegates: tc.delegates}.checkDelegates(conf)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkDelegates = %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

func TestServeCommandCoversEveryCommandTheTransportAccepts(t *testing.T) {
	pods, _ := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	reader := nodePodReader{pods: pods, api: api}
	// DEL without a network namespace is the lenient teardown path, so it
	// exercises dispatch and the success payload without touching hardware.
	for _, command := range []string{daemon.DelCommand, daemon.CheckCommand} {
		result, err := serveCommand(context.Background(), reader, command, &skel.CmdArgs{StdinData: []byte(`{}`)})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if !json.Valid(result) || len(result) == 0 {
			t.Fatalf("%s produced %q, want a payload the transport accepts", command, result)
		}
	}
	if _, err := serveCommand(context.Background(), reader, "GC",
		&skel.CmdArgs{StdinData: []byte(`{}`)}); err == nil {
		t.Fatal("daemon served a command it does not implement")
	}
}

func TestHandlerPreservesThePluginErrorCode(t *testing.T) {
	pods, _ := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	response, err := cniHandler(nodePodReader{pods: pods, api: api})(context.Background(),
		&daemon.Request{Command: "GC", IfName: "ib1"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || len(response.Result) != 0 {
		t.Fatalf("response = %+v, want exactly one error", response)
	}
}

func TestDaemonRouteReadsTheSocketAndTheBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		config      string
		wantSocket  string
		wantTimeout time.Duration
	}{
		{name: "no daemon", config: `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov"}`},
		{name: "unreadable", config: `{`},
		{
			name:        "daemon without handshake",
			config:      `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","daemonSocketPath":"/run/x/d.sock"}`,
			wantSocket:  "/run/x/d.sock",
			wantTimeout: daemonCallOverhead,
		},
		{
			name: "daemon with handshake",
			config: `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","daemonSocketPath":"/run/x/d.sock",` +
				`"ibKubernetesEnabled":true,"publishPfGuid":true,"readinessTimeoutSeconds":120}`,
			wantSocket:  "/run/x/d.sock",
			wantTimeout: daemonCallOverhead + 120*time.Second,
		},
		{
			// The call has to outlast the default wait too, or a configuration
			// that does not size the handshake is cut off mid-wait.
			name: "daemon with handshake, default budget",
			config: `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","daemonSocketPath":"/run/x/d.sock",` +
				`"ibKubernetesEnabled":true,"publishPfGuid":true}`,
			wantSocket:  "/run/x/d.sock",
			wantTimeout: daemonCallOverhead + localtypes.DefaultReadinessTimeoutSeconds*time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket, timeout, _ := daemonRoute([]byte(tc.config))
			if socket != tc.wantSocket || timeout != tc.wantTimeout {
				t.Fatalf("daemonRoute = (%q, %v), want (%q, %v)", socket, timeout, tc.wantSocket, tc.wantTimeout)
			}
		})
	}
}

func startStubDaemon(t *testing.T, handler daemon.Handler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "run", "ib-sriov.sock")
	if err := prepareSocketDir(socket); err != nil {
		t.Fatal(err)
	}
	server, err := daemon.NewServer(socket, 5*time.Second, 4, handler)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return socket
}

func TestForwardCommandCarriesTheInvocationAndReturnsTheResult(t *testing.T) {
	var seen daemon.Request
	socket := startStubDaemon(t, func(_ context.Context, request *daemon.Request) (*daemon.Response, error) {
		seen = *request
		return &daemon.Response{Result: json.RawMessage(`{"cniVersion":"1.0.0"}`)}, nil
	})
	args := &skel.CmdArgs{
		ContainerID: "cid", Netns: "/proc/1/ns/net", IfName: "ib1",
		Args: "K8S_POD_NAME=vm-1", Path: "/opt/cni/bin", StdinData: []byte(`{"name":"net-a"}`),
	}
	result, err := forwardCommand(context.Background(), args, daemon.AddCommand, socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"cniVersion":"1.0.0"}` {
		t.Fatalf("result = %s", result)
	}
	if seen.Command != daemon.AddCommand || seen.ContainerID != args.ContainerID || seen.Netns != args.Netns ||
		seen.IfName != args.IfName || seen.Args != args.Args || seen.Path != args.Path ||
		!bytes.Equal(seen.StdinData, args.StdinData) {
		t.Fatalf("daemon received %+v, want the fields of %+v", seen, args)
	}
}

func TestForwardCommandReturnsThePluginErrorUnchanged(t *testing.T) {
	socket := startStubDaemon(t, func(context.Context, *daemon.Request) (*daemon.Response, error) {
		return daemon.Failure(cnitypes.NewError(cnitypes.ErrTryAgainLater, "not ready", "wait")), nil
	})
	_, err := forwardCommand(context.Background(), &skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: []byte(`{}`)},
		daemon.AddCommand, socket, 5*time.Second)
	var cniErr *cnitypes.Error
	if !errors.As(err, &cniErr) {
		t.Fatalf("forwardCommand error = %v, want a CNI error", err)
	}
	if cniErr.Code != cnitypes.ErrTryAgainLater || cniErr.Msg != "not ready" || cniErr.Details != "wait" {
		t.Fatalf("CNI error = %+v, want the plugin's own code and message", cniErr)
	}
}

func TestPrepareSocketDirIsPrivateAndRepeatable(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "run", "ib-sriov.sock")
	for _, mode := range []os.FileMode{0755, 0700} {
		if err := prepareSocketDir(socket); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Dir(socket))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("socket directory mode = %v, want 0700", info.Mode().Perm())
		}
		// A directory left behind by an earlier run must be usable again.
		if err = os.Chmod(filepath.Dir(socket), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDaemonOptionsRequireANodeAndBoundedLimits(t *testing.T) {
	valid := daemonOptions{
		node: testNodeName, socket: "/run/ib/ib-sriov.sock",
		syncTimeout: time.Second, requestTimeout: time.Second, maxInFlight: 1,
	}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*daemonOptions){
		"no node":                       func(o *daemonOptions) { o.node = "  " },
		"relative socket":               func(o *daemonOptions) { o.socket = "ib-sriov.sock" },
		"no sync budget":                func(o *daemonOptions) { o.syncTimeout = 0 },
		"no request limit":              func(o *daemonOptions) { o.requestTimeout = 0 },
		"no capacity":                   func(o *daemonOptions) { o.maxInFlight = 0 },
		"relative install directory":    func(o *daemonOptions) { o.installBinDir = "opt/cni/bin" },
		"kubeconfig without a conf dir": func(o *daemonOptions) { o.genKubeconfig = true; o.cniConfDir = "" },
	} {
		t.Run(name, func(t *testing.T) {
			opt := valid
			mutate(&opt)
			if err := opt.validate(); err == nil {
				t.Fatal("invalid daemon options accepted")
			}
		})
	}
}

// runTestDaemon starts one daemon and returns its socket and a wait function
// that reports why it stopped. The wait is idempotent so a test can consume the
// outcome and the cleanup can still confirm the daemon is gone.
func runTestDaemon(t *testing.T, client *fake.Clientset, syncTimeout time.Duration) (socket string, wait func() error) {
	return runTestDaemonWith(t, client, syncTimeout, func(*daemonOptions) {})
}

func runTestDaemonWith(t *testing.T, client *fake.Clientset, syncTimeout time.Duration,
	customize func(*daemonOptions)) (socket string, wait func() error) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "run", "ib-sriov.sock")
	opt := daemonOptions{
		node: testNodeName, socket: socket, syncTimeout: syncTimeout,
		requestTimeout: 5 * time.Second, maxInFlight: 4,
	}
	customize(&opt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, opt, client) }()
	var once sync.Once
	var result error
	wait = func() error {
		once.Do(func() {
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				result = errors.New("daemon did not stop")
				t.Error(result)
			}
		})
		return result
	}
	t.Cleanup(func() { cancel(); _ = wait() })
	return socket, wait
}

func TestRunDaemonServesTheSocketUntilItIsCanceled(t *testing.T) {
	socket, _ := runTestDaemon(t, fake.NewClientset(cachedPod()), 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := forwardCommand(context.Background(), &skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: []byte(`{}`)},
			daemon.CheckCommand, socket, 5*time.Second)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never answered on its socket: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitSocket returns once the daemon answers a CHECK on its socket.
func awaitSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := forwardCommand(context.Background(), &skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: []byte(`{}`)},
			daemon.CheckCommand, socket, 5*time.Second)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never answered on its socket: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The daemon is the DaemonSet's only container, so before it answers on its
// socket the host must hold this very executable as the plugin: a runtime that
// execs the plugin once the socket is up must get the same code it forwards to.
func TestRunDaemonInstallsThePluginBeforeServing(t *testing.T) {
	binDir := t.TempDir()
	socket, _ := runTestDaemonWith(t, fake.NewClientset(cachedPod()), 5*time.Second,
		func(opt *daemonOptions) { opt.installBinDir = binDir })
	awaitSocket(t, socket)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(binDir, "ib-sriov"))
	if err != nil {
		t.Fatalf("plugin was not installed before the socket served: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("installed plugin differs from the serving executable")
	}
}

// An install that cannot reach the host directory must stop the daemon before
// it serves: a node with a serving socket and no plugin is worse than a node
// with neither, because the failure then surfaces on the first machine start.
func TestRunDaemonStopsWhenThePluginCannotBeInstalled(t *testing.T) {
	_, wait := runTestDaemonWith(t, fake.NewClientset(cachedPod()), 5*time.Second,
		func(opt *daemonOptions) { opt.installBinDir = filepath.Join(t.TempDir(), "absent") })
	if err := wait(); err == nil || !strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("daemon ran without installing the plugin: %v", err)
	}
}

func TestRunDaemonRefusesToServeAnUnsynchronizedCache(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list refused")
	})
	_, wait := runTestDaemon(t, client, 100*time.Millisecond)
	if err := wait(); err == nil {
		t.Fatal("daemon served requests from a cache that never synchronized")
	}
}

// routedConfig is the configuration that sends one attachment to a daemon.
func routedConfig(socket string) []byte {
	return []byte(`{"cniVersion":"1.0.0","name":"net-a","type":"ib-sriov",` +
		`"deviceID":"0000:03:00.0","daemonSocketPath":"` + socket + `"}`)
}

func TestCommandsReachTheDaemonInsteadOfRunningLocally(t *testing.T) {
	served := make(chan string, 4)
	socket := startStubDaemon(t, func(_ context.Context, request *daemon.Request) (*daemon.Response, error) {
		served <- request.Command
		return &daemon.Response{Result: json.RawMessage(`{"cniVersion":"1.0.0"}`)}, nil
	})
	args := &skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: routedConfig(socket)}
	if err := cmdDel(args); err != nil {
		t.Fatal(err)
	}
	if err := cmdCheck(args); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{daemon.DelCommand, daemon.CheckCommand} {
		select {
		case got := <-served:
			if got != want {
				t.Fatalf("daemon served %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never reached the daemon", want)
		}
	}
}

func TestAddWritesTheResultTheDaemonRendered(t *testing.T) {
	const rendered = `{"cniVersion":"1.0.0","interfaces":[{"name":"ib1"}]}`
	socket := startStubDaemon(t, func(context.Context, *daemon.Request) (*daemon.Response, error) {
		return &daemon.Response{Result: json.RawMessage(rendered)}, nil
	})
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = write
	addErr := cmdAdd(&skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: routedConfig(socket)})
	os.Stdout = saved
	if err = write.Close(); err != nil {
		t.Fatal(err)
	}
	if addErr != nil {
		t.Fatal(addErr)
	}
	printed, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon holds the configuration's CNI version, so its bytes are
	// forwarded unchanged rather than re-rendered by this process.
	if string(printed) != rendered+"\n" {
		t.Fatalf("stdout = %q, want %q", printed, rendered+"\n")
	}
}

func TestLockAcquisitionStopsWhenTheCallerIsGone(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireLock(context.Background(), dir, "cni.lock")
	if err != nil {
		t.Fatal(err)
	}
	// A daemon slot must not be held by a request whose caller has given up.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err = acquireLock(ctx, dir, "cni.lock"); err == nil {
		t.Fatal("a second holder acquired the lock")
	}
	if waited := time.Since(started); waited < 100*time.Millisecond {
		t.Fatalf("gave up after %v without waiting for the holder", waited)
	}
	unlockCNIExecution(held)
	released, err := acquireLock(context.Background(), dir, "cni.lock")
	if err != nil {
		t.Fatalf("lock not reusable after release: %v", err)
	}
	unlockCNIExecution(released)
}

func TestLogVerbosityReachesKlogAndRejectsNegative(t *testing.T) {
	t.Cleanup(func() {
		if err := applyLogVerbosity(0); err != nil {
			t.Fatal(err)
		}
	})
	if klog.V(6).Enabled() {
		t.Fatal("verbosity 6 is on before it was asked for")
	}
	if err := applyLogVerbosity(6); err != nil {
		t.Fatal(err)
	}
	// client-go installs its request-logging transport only when this is on,
	// and it decides once, when the client is built.
	if !klog.V(6).Enabled() {
		t.Fatal("log-level did not reach klog")
	}
	if err := applyLogVerbosity(-1); err == nil {
		t.Fatal("negative log level accepted")
	}
}

func TestWaitDoesNotTrustAStalledCacheForever(t *testing.T) {
	pods, _ := startNodeCache(t, cachedPod())
	api, _ := newAnnotationTest(t)
	// The cache has the Pod, so nothing forces a live read on its own. This
	// is the state a silently dead watch leaves behind: the copy is there and
	// it is wrong.
	reader := newRecheckingReader(nodePodReader{pods: pods, api: api}, 50*time.Millisecond)
	if _, err := reader.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 0 {
		t.Fatalf("live reads inside the interval = %d, want 0", api.getCalls)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := reader.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 1 {
		t.Fatalf("live reads after the interval = %d, want 1", api.getCalls)
	}
	// And the clock restarts, so the next poll is cheap again.
	if _, err := reader.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 1 {
		t.Fatalf("live reads = %d, want the interval to have restarted", api.getCalls)
	}
}

func TestAPodTheCacheNeverLearnsIsStillReadCheaply(t *testing.T) {
	// The Pod exists, but this node's informer never sees it. Every poll misses
	// and must fall back, or the wait would declare the Pod gone after five.
	// The fallback is what has to stay bounded.
	pods, _ := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	reader := newRecheckingReader(nodePodReader{pods: pods, api: api}, 100*time.Millisecond)
	for range 25 {
		pod, err := reader.GetPod(context.Background(), "ns1", "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		if pod.Name != "vm-1" {
			t.Fatalf("read %q, want vm-1", pod.Name)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 25 polls over roughly 250ms at a 100ms interval: a handful of reads, not
	// one per poll.
	if api.getCalls > 5 {
		t.Fatalf("live reads = %d over 25 polls, want the interval to bound them", api.getCalls)
	}
	if api.getCalls == 0 {
		t.Fatal("a Pod absent from the cache was never read live")
	}
}

func TestARememberedAbsenceStillEndsTheWait(t *testing.T) {
	pods, _ := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	api.getErr = apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	reader := newRecheckingReader(nodePodReader{pods: pods, api: api}, time.Hour)
	// A Pod that is gone must keep reading as gone between checks, so the
	// waiter's consecutive-miss limit still fires instead of stalling.
	for i := range 5 {
		if _, err := reader.GetPod(context.Background(), "ns1", "vm-1"); !apierrors.IsNotFound(err) {
			t.Fatalf("poll %d = %v, want NotFound", i, err)
		}
	}
	if api.getCalls != 1 {
		t.Fatalf("live reads = %d, want the absence remembered after 1", api.getCalls)
	}
}

func TestADisconnectedCallerCancelsTheHandler(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan error, 1)
	socket := startStubDaemon(t, func(ctx context.Context, _ *daemon.Request) (*daemon.Response, error) {
		close(started)
		<-ctx.Done()
		observed <- ctx.Err()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := daemon.NewClient(socket)
	defer client.Close()
	callCtx, abandon := context.WithCancel(ctx)
	go func() {
		<-started
		// The runtime gave up on this command. The daemon must stop working
		// on it rather than hold a slot for the rest of its deadline.
		abandon()
	}()
	callDeadline, stop := context.WithDeadline(callCtx, time.Now().Add(4*time.Second))
	defer stop()
	_, err := client.Call(callDeadline, daemon.Request{
		Command: daemon.AddCommand, ContainerID: "cid", IfName: "ib1", StdinData: []byte(`{}`)})
	if err == nil {
		t.Fatal("abandoned call reported success")
	}
	select {
	case got := <-observed:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("handler saw %v, want cancellation", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler was never canceled by the caller going away")
	}
}

func TestAFailedDaemonCallIsNotRetriedInProcess(t *testing.T) {
	// Falling back to in-process execution after a daemon error would run the
	// device work twice: the daemon may already have rebound hardware or
	// published a record before it failed.
	socket := startStubDaemon(t, func(context.Context, *daemon.Request) (*daemon.Response, error) {
		return daemon.Failure(cnitypes.NewError(cnitypes.ErrTryAgainLater, "busy", "")), nil
	})
	args := &skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: routedConfig(socket)}
	err := cmdDel(args)
	var cniErr *cnitypes.Error
	if !errors.As(err, &cniErr) || cniErr.Code != cnitypes.ErrTryAgainLater {
		t.Fatalf("cmdDel = %v, want the daemon's own error surfaced", err)
	}
}

// daemonSocketPath cannot be the only way to reach the daemon: definitions are
// generated by tenant onboarding where this ships, so the well-known socket is
// used when it exists and the command runs in-process when it does not.
func TestDaemonRouteDiscoversTheWellKnownSocket(t *testing.T) {
	base := `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","ibKubernetesEnabled":true,"publishPfGuid":true}`
	socket, timeout, _ := daemonRoute([]byte(base))
	if socket != "" || timeout != 0 {
		t.Fatalf("routed to an absent daemon: %q %v", socket, timeout)
	}

	// Present it at the well-known path and the same configuration forwards.
	dir := t.TempDir()
	path := filepath.Join(dir, "ib-sriov.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	explicit := `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","ibKubernetesEnabled":true,` +
		`"publishPfGuid":true,"daemonSocketPath":"` + path + `"}`
	socket, timeout, _ = daemonRoute([]byte(explicit))
	if socket != path {
		t.Fatalf("explicit socket ignored: %q", socket)
	}
	if want := daemonCallOverhead + localtypes.DefaultReadinessTimeoutSeconds*time.Second; timeout != want {
		t.Fatalf("budget = %v, want %v", timeout, want)
	}
}

// The GUID is read from the device once per function per daemon lifetime;
// every later ADD, including after a rollback retry, is answered from memory.
// The index is recomputed on a hit because it also proves the address still
// holds an InfiniBand function.
func TestCachedPFDetailReadsTheDeviceOnceThenAnswersFromMemory(t *testing.T) {
	cache := guidcache.New()
	var reads, indexes int
	index := func(string) (int, error) { indexes++; return 2, nil }
	read := func(string, string) (int, string, error) { reads++; return 2, "e0:9d:73:03:00:fe:b0:a0", nil }
	ctx, request := observability.Start(context.Background(), "daemon", daemon.AddCommand, nil)
	for i := 0; i < 3; i++ {
		idx, guid, err := cachedPFDetail(ctx, cache, "0010:03:00.0", "", index, read)
		if err != nil || idx != 2 || guid != "e0:9d:73:03:00:fe:b0:a0" {
			t.Fatalf("pass %d: %d %q %v", i, idx, guid, err)
		}
	}
	if reads != 1 || indexes != 2 {
		t.Fatalf("device reads=%d (want 1), index checks on hits=%d (want 2)", reads, indexes)
	}
	// The two hits are visible as guid_cache phases; the miss recorded none
	// (its device_lock and device_inspection come from the real reader).
	var hits int
	for _, phase := range request.Snapshot().Phases {
		if phase.Name == "guid_cache" && phase.Result == observability.ResultSuccess {
			hits++
		}
	}
	if hits != 2 {
		t.Fatalf("guid_cache phases = %d, want 2", hits)
	}
}

// A seeded entry is a hit from the first ADD; a cached address that no longer
// holds an InfiniBand function is refused; and without a cache, as in the
// plugin process, the device is read every time.
func TestCachedPFDetailSeededRefusedAndUncached(t *testing.T) {
	ctx := context.Background()
	var reads int
	index := func(string) (int, error) { return 0, nil }
	read := func(string, string) (int, string, error) { reads++; return 0, "e0:9d:73:03:00:fe:b0:a0", nil }
	seeded := guidcache.New()
	seeded.Store("0000:03:00.0", "e0:9d:73:03:00:fe:b0:a1")
	if _, guid, err := cachedPFDetail(ctx, seeded, "0000:03:00.0", "", index, read); err != nil || guid != "e0:9d:73:03:00:fe:b0:a1" || reads != 0 {
		t.Fatalf("seeded entry not used: %q %v reads=%d", guid, err, reads)
	}
	bad := func(string) (int, error) { return 0, errors.New("not an InfiniBand controller") }
	if _, _, err := cachedPFDetail(ctx, seeded, "0000:03:00.0", "", bad, read); err == nil {
		t.Fatal("cached GUID served for an address that failed the index check")
	}
	for i := 0; i < 2; i++ {
		if _, _, err := cachedPFDetail(ctx, nil, "0010:03:00.0", "", index, read); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 {
		t.Fatalf("nil cache read the device %d times, want 2", reads)
	}
}

// A GUID the Pod's retained Ready carries is used ahead of the cache and the
// device, and it seeds the cache so a daemon restarted mid-retry learns it.
func TestCachedPFDetailTrustsTheRetainedGUIDAndSeedsTheCache(t *testing.T) {
	ctx := context.Background()
	cache := guidcache.New()
	cache.Store("0000:03:00.0", "e0:9d:73:03:00:fe:b0:01") // stale or different: the Pod's own record wins
	var reads int
	index := func(string) (int, error) { return 0, nil }
	read := func(string, string) (int, string, error) { reads++; return 0, "e0:9d:73:03:00:fe:b0:ff", nil }
	_, guid, err := cachedPFDetail(ctx, cache, "0000:03:00.0", "e0:9d:73:03:00:fe:b0:a0", index, read)
	if err != nil || guid != "e0:9d:73:03:00:fe:b0:a0" || reads != 0 {
		t.Fatalf("retained GUID not used: %q %v reads=%d", guid, err, reads)
	}
	if got, _ := cache.Lookup("0000:03:00.0"); got != "e0:9d:73:03:00:fe:b0:a0" {
		t.Fatalf("cache not seeded from the retained GUID: %q", got)
	}
	bad := func(string) (int, error) { return 0, errors.New("not an InfiniBand controller") }
	if _, _, err := cachedPFDetail(ctx, nil, "0000:03:00.0", "e0:9d:73:03:00:fe:b0:a0", bad, read); err == nil {
		t.Fatal("retained GUID served for an address that failed the index check")
	}
}
