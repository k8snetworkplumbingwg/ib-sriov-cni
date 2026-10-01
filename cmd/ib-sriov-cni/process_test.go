package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cniconfig "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
)

// These tests run the shipped binary as a real child process against a
// synthetic localhost API. They cover what an in-process test cannot: startup,
// signal handling, what the process inherits, and what it writes to stdout.
//
// The binary is built by TestMain rather than located through the environment.
// A missing binary has to fail the run; a test that quietly skips is the same
// as no test, and this coverage exists precisely because it is easy to lose.

var shippedBinary string

func TestMain(m *testing.M) {
	if path := os.Getenv(pluginLogHelperEnv); path != "" {
		// A second plugin process for TestPluginLogSurvivesASecondProcess:
		// open the shared log the way the plugin does and write one line.
		file, err := openPluginLog(path, 1, 3, 7)
		if err == nil {
			_, err = file.WriteString("SECOND-PROCESS-LINE\n")
		}
		if err == nil {
			err = file.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "log writer helper:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "ib-sriov-process-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "process tests need a temporary directory:", err)
		os.Exit(1)
	}
	shippedBinary = filepath.Join(dir, "ib-sriov")
	// Node-local state this process writes: the CNI cache and the lock
	// directory. Tests own them here; cacheDirAt narrows them per test.
	cniconfig.DefaultCNIDir = filepath.Join(dir, "cache")
	cniconfig.CniFileLockDir = filepath.Join(dir, "locks")
	build := exec.Command("go", "build", "-o", shippedBinary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "process tests could not build the shipped binary:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestProcessRefusesToStartWithoutANode(t *testing.T) {
	process := startProcess(t, shippedBinary, nil, "", "--daemon", "--socket=/run/absent/ib-sriov.sock")
	awaitProcess(t, process)
	if process.err == nil {
		t.Fatal("daemon started with no node name")
	}
	if !strings.Contains(process.stderr.String(), "node name") {
		t.Fatalf("stderr = %q, want it to name the missing node", process.stderr.String())
	}
}

func TestProcessColdSyncFailureCancelsItsAPIRequest(t *testing.T) {
	api := newProcessAPI(t, true)
	socket, kubeconfig := processFiles(t, api.server.URL)
	process := startProcess(t, shippedBinary, nil, "", "--daemon", "--node-name=node-a",
		"--socket="+socket, "--kubeconfig="+kubeconfig, "--sync-timeout=300ms")
	awaitAPIStart(t, api, process)
	awaitProcess(t, process)
	if process.err == nil {
		t.Fatal("daemon served requests after its cache failed to synchronize")
	}
	if !strings.Contains(process.stderr.String(), "initial synchronization") {
		t.Fatalf("stderr = %q, want the synchronization failure named", process.stderr.String())
	}
	// The in-flight request must be canceled, not abandoned to the API server.
	awaitSignal(t, api.stopped, "initial API request cancellation")
	if counts := api.counts(); counts.invalid != 0 {
		t.Fatalf("API saw %d unfiltered requests, want the node field selector on every one", counts.invalid)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket survived a failed startup: %v", err)
	}
}

func TestProcessTerminationStopsTheWatchAndRemovesItsSocket(t *testing.T) {
	api := newProcessAPI(t, false)
	socket, kubeconfig := processFiles(t, api.server.URL)
	process := startProcess(t, shippedBinary, nil, "", "--daemon", "--node-name=node-a",
		"--socket="+socket, "--kubeconfig="+kubeconfig, "--sync-timeout=5s")
	awaitAPIStart(t, api, process)
	awaitServing(t, process)
	select {
	case <-api.stopped:
		t.Fatal("watch stopped before the daemon was asked to")
	default:
	}
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitProcess(t, process)
	if process.err != nil {
		t.Fatalf("termination was not clean: %v: %s", process.err, process.stderr.String())
	}
	awaitSignal(t, api.stopped, "watch cancellation during shutdown")
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket outlived the daemon: %v", err)
	}
}

func TestProcessPluginForwardsItsInvocationAndPrintsATypedError(t *testing.T) {
	api := newProcessAPI(t, false)
	socket, kubeconfig := processFiles(t, api.server.URL)
	daemonProcess := startProcess(t, shippedBinary, nil, "", "--daemon", "--node-name=node-a",
		"--socket="+socket, "--kubeconfig="+kubeconfig, "--sync-timeout=5s")
	awaitAPIStart(t, api, daemonProcess)
	awaitServing(t, daemonProcess)

	// A configuration the daemon rejects before it reaches any device. What is
	// under test is the path: the plugin process reads its command from the
	// environment and its configuration from stdin, forwards both, and renders
	// whatever came back as a CNI error on stdout.
	config := fmt.Sprintf(
		`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","daemonSocketPath":%q}`, socket)
	plugin := startProcess(t, shippedBinary, []string{
		"CNI_COMMAND=ADD", "CNI_CONTAINERID=synthetic", "CNI_IFNAME=ib1",
		"CNI_NETNS=/not-a-real-netns", "CNI_PATH=/opt/cni/bin",
		"CNI_ARGS=K8S_POD_NAMESPACE=tenant;K8S_POD_NAME=a",
	}, config)
	awaitProcess(t, plugin)
	if plugin.err == nil {
		t.Fatalf("plugin reported success: %s", plugin.stdout.String())
	}
	var cniError cnitypes.Error
	if err := json.Unmarshal([]byte(plugin.stdout.String()), &cniError); err != nil {
		t.Fatalf("stdout was not a CNI error: %q: %v", plugin.stdout.String(), err)
	}
	if cniError.Code == 0 || cniError.Msg == "" {
		t.Fatalf("CNI error is not usable: %+v", cniError)
	}
	if !strings.Contains(cniError.Msg+cniError.Details, "deviceID") {
		t.Fatalf("CNI error = %+v, want the configuration fault the daemon found", cniError)
	}
	// The daemon reached that verdict from its own cache.
	if gets := api.counts().gets; gets != 0 {
		t.Fatalf("daemon made %d live reads for one rejected command, want 0", gets)
	}
	awaitCompletionSummaries(t, daemonProcess, 1)
	for _, log := range []string{plugin.stderr.String(), daemonProcess.stderr.String()} {
		if strings.Count(log, "CNI request completed") != 1 || !strings.Contains(log, `result="error"`) {
			t.Fatalf("missing/duplicate failure summary: %s", log)
		}
	}
}

func processFiles(t *testing.T, serverURL string) (socket, kubeconfig string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: %s
users:
- name: local
  user: {}
contexts:
- name: local
  context:
    cluster: local
    user: local
current-context: local
`, serverURL)
	kubeconfig = filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "ib-sriov.sock"), kubeconfig
}

// syncBuffer collects a child's output while it runs. A plain buffer would be
// written by os/exec's goroutine and read by the test at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type childProcess struct {
	cmd            *exec.Cmd
	stdout, stderr syncBuffer
	done           chan struct{}
	err            error // Read only after done closes.
}

func startProcess(t *testing.T, binary string, env []string, stdin string, args ...string) *childProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	process := &childProcess{cmd: exec.CommandContext(ctx, binary, args...), done: make(chan struct{})}
	// Inherit no credentials, kubeconfig, proxy or CNI argument from the host,
	// so what the child sees is only what this test gave it.
	process.cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	process.cmd.Stdin = strings.NewReader(stdin)
	process.cmd.Stdout, process.cmd.Stderr = &process.stdout, &process.stderr
	if err := process.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() {
		process.err = process.cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-process.done:
		case <-time.After(10 * time.Second):
			t.Error("child process did not exit after cancellation")
		}
	})
	return process
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitProcess(t *testing.T, process *childProcess) {
	t.Helper()
	awaitSignal(t, process.done, "child process completion")
}

func awaitAPIStart(t *testing.T, api *processAPI, process *childProcess) {
	t.Helper()
	select {
	case <-api.started:
	case <-process.done:
		t.Fatalf("daemon exited before reaching the API: %v: %s", process.err, process.stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon never reached the API; counts: %+v", api.counts())
	}
}

// awaitServing waits for the daemon to announce that it is serving. The socket
// appears earlier, before the cache has synchronized, so its existence is not
// the signal a caller or a test should key on.
func awaitServing(t *testing.T, process *childProcess) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(process.stderr.String(), "daemon serving") {
			return
		}
		select {
		case <-process.done:
			t.Fatalf("daemon exited before serving: %v: %s", process.err, process.stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("daemon never reported serving: %s", process.stderr.String())
}

type apiCounts struct{ lists, watches, gets, invalid, writeErrors int }

// processAPI is a synthetic API server that counts what the daemon asks of it
// and can be told to hang, so a cold-sync failure is reproducible.
type processAPI struct {
	server  *httptest.Server
	mu      sync.Mutex
	totals  apiCounts
	hang    bool
	started chan struct{}
	stopped chan struct{}
	start   sync.Once
	stop    sync.Once
}

func newProcessAPI(t *testing.T, hang bool) *processAPI {
	t.Helper()
	api := &processAPI{hang: hang, started: make(chan struct{}), stopped: make(chan struct{})}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	t.Cleanup(api.server.Close)
	return api
}

func (a *processAPI) counts() apiCounts {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totals
}

func (a *processAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	watching := r.URL.Query().Get("watch") == "true"
	a.mu.Lock()
	switch {
	case r.URL.Path != "/api/v1/pods":
		a.totals.gets++
	case watching:
		a.totals.watches++
	default:
		a.totals.lists++
	}
	valid := r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods" &&
		r.URL.Query().Get("fieldSelector") == "spec.nodeName=node-a"
	if !valid {
		a.totals.invalid++
	}
	a.mu.Unlock()
	if !valid {
		http.Error(w, "unexpected request or missing node filter", http.StatusBadRequest)
		return
	}
	if a.hang {
		a.start.Do(func() { close(a.started) })
		<-r.Context().Done()
		a.stop.Do(func() { close(a.stopped) })
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !watching {
		err := json.NewEncoder(w).Encode(&corev1.PodList{
			TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
			ListMeta: metav1.ListMeta{ResourceVersion: "1"},
			Items:    []corev1.Pod{*processPod("a", "1")}})
		if err != nil {
			a.mu.Lock()
			a.totals.writeErrors++
			a.mu.Unlock()
		}
		return
	}
	a.serveWatch(w, r)
}

func (a *processAPI) serveWatch(w http.ResponseWriter, r *http.Request) {
	defer a.stop.Do(func() { close(a.stopped) })
	encoder := json.NewEncoder(w)
	send := func(kind string, pod *corev1.Pod) error {
		err := encoder.Encode(struct {
			Type   string      `json:"type"`
			Object *corev1.Pod `json:"object"`
		}{kind, pod})
		w.(http.Flusher).Flush()
		return err
	}
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		if err := send("ADDED", processPod("a", "1")); err != nil {
			return
		}
		bookmark := processPod("", "1")
		bookmark.Annotations = map[string]string{"k8s.io/initial-events-end": "true"}
		if err := send("BOOKMARK", bookmark); err != nil {
			return
		}
	}
	w.(http.Flusher).Flush()
	a.start.Do(func() { close(a.started) })
	<-r.Context().Done()
}

func processPod(name, revision string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant", Name: name, UID: types.UID(name), ResourceVersion: revision,
			Annotations: map[string]string{"v1.ib-sriov-cni.io/connection-details": "{}"}},
		Spec: corev1.PodSpec{NodeName: "node-a"},
	}
}
