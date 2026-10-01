package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/guidcache"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/install"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/podcache"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

const (
	// daemonSyncTimeout bounds the initial Pod cache synchronization. A daemon
	// that cannot list its node's Pods exits rather than serve commands that
	// would read an empty cache and conclude the Pod is gone.
	daemonSyncTimeout = 30 * time.Second

	// daemonCallOverhead is what a forwarded command is allowed beyond its
	// readiness budget: transport, the PF inspection rebind, and the wait for
	// the CNI file lock that serializes it against every other attachment.
	daemonCallOverhead = 2 * time.Minute

	// daemonRequestCeiling is the longest a single request may hold a slot.
	// The caller states its own deadline and that is the real budget; this
	// only stops a caller that disappears from holding a slot forever, so it
	// covers the longest budget a configuration can ask for, with a margin.
	daemonRequestCeiling = daemonCallOverhead +
		time.Duration(localtypes.MaxReadinessTimeoutSeconds)*time.Second + time.Minute

	// daemonMaxInFlight caps concurrent requests. Every attachment of every
	// Pod starting on the node holds one slot for the length of its readiness
	// wait, so this is sized for Pods times interfaces, not for Pods.
	daemonMaxInFlight = 256

	daemonDirectoryMode = 0700

	// cacheRecheckInterval bounds how far behind the API server one readiness
	// wait can run. An informer's watch can stop delivering without saying so:
	// on 2026-09-16 a hardware round lost one to a dropped HTTP/2 connection
	// and client-go took 45 seconds to notice, during which the cache kept
	// answering with a copy that predated the controller's response and the
	// wait had no way to tell. Initial synchronization proves nothing about
	// this, so a wait re-reads the source at this interval. It is the cost of
	// the cache being an optimization rather than a source of truth: a wait of
	// a few seconds still costs about one read instead of the twenty-five that
	// polling the API server outright would.
	cacheRecheckInterval = 5 * time.Second
)

type daemonOptions struct {
	enabled            bool
	node               string
	socket             string
	kubeconfig         string
	syncTimeout        time.Duration
	requestTimeout     time.Duration
	maxInFlight        int
	logLevel           int
	metricsBindAddress string

	logFile           string
	logFileMaxSize    int
	logFileMaxBackups int
	logFileMaxAge     int

	// The daemon is the DaemonSet's only container, so it also puts the plugin
	// on the node: installBinDir receives a copy of this executable, and with
	// genKubeconfig the node-local kubeconfig the in-process plugin uses is
	// written under cniConfDir and kept ahead of token rotation.
	installBinDir string
	cniConfDir    string
	genKubeconfig bool
}

// applyLogVerbosity routes a level into klog's own flag, which is the only
// way klog exposes verbosity. This binary cannot register klog's flags
// directly: -v has been the plugin's version shorthand since before there was
// a daemon, and taking it away would break every existing invocation. It must
// run before any client is built, because client-go decides once, at
// construction, whether to install its request-logging transport.
func applyLogVerbosity(level int) error {
	if level < 0 {
		return errors.New("log level cannot be negative")
	}
	flags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(flags)
	return flags.Set("v", strconv.Itoa(level))
}

func (opt daemonOptions) validate() error {
	if strings.TrimSpace(opt.node) == "" {
		return errors.New("daemon mode requires a node name: pass -node-name or set NODE_NAME")
	}
	if !filepath.IsAbs(opt.socket) || opt.syncTimeout <= 0 || opt.requestTimeout <= 0 || opt.maxInFlight <= 0 {
		return errors.New("daemon mode requires an absolute socket path and positive limits")
	}
	if opt.logLevel < 0 {
		return errors.New("daemon mode requires a non-negative log level")
	}
	if opt.installBinDir != "" && !filepath.IsAbs(opt.installBinDir) {
		return errors.New("--install-cni-bin-dir must be an absolute path")
	}
	if opt.genKubeconfig && !filepath.IsAbs(opt.cniConfDir) {
		return errors.New("--gen-kubeconfig requires an absolute --cni-conf-dir")
	}
	if opt.logFile != "" && strings.HasPrefix(filepath.Clean(opt.logFile), pluginLogDir+string(filepath.Separator)) {
		// The plugin's directory is the one place a network definition can
		// name a file; a daemon log in there could be rotated by a plugin call.
		return fmt.Errorf("--log-file must not be under %s, the plugin's log directory", pluginLogDir)
	}
	return nil
}

// installPlugin puts this executable and, if asked, the node kubeconfig on
// the host before the daemon serves, so the runtime never execs a plugin
// newer or older than the daemon it forwards to. Returns the refresh loop to
// run alongside serving, or nil.
func installPlugin(opt daemonOptions) (func(context.Context), error) {
	if opt.installBinDir != "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cannot locate this executable to install it: %w", err)
		}
		dst, err := install.CopyBinary(self, opt.installBinDir)
		if err != nil {
			return nil, err
		}
		klog.InfoS("installed plugin binary", "path", dst, "commit", commit)
	}
	if !opt.genKubeconfig {
		return nil, nil
	}
	dst, err := install.WriteKubeconfig(opt.cniConfDir)
	if err != nil {
		return nil, err
	}
	klog.InfoS("wrote node kubeconfig", "path", dst)
	return func(ctx context.Context) {
		install.RefreshKubeconfig(ctx, opt.cniConfDir, install.KubeconfigRefreshInterval, func(err error) {
			klog.ErrorS(err, "kubeconfig refresh failed; the in-process plugin will lose API access when the token rotates")
		})
	}, nil
}

// nodePodReader answers Pod reads from the node-local informer, so a served
// command costs the API server no read. A miss falls back to a single live
// read: a Pod can be scheduled onto this node before its first watch event
// arrives here, and the readiness wait bounds how many consecutive misses it
// tolerates before it declares the Pod gone.
type nodePodReader struct {
	pods *podcache.Cache
	api  podAnnotationClient
	// guids is the daemon's memory of each function's GUID; nil reads the
	// device every time, which is what a plugin process without a daemon does.
	guids *guidcache.Cache
}

// deviceReader resolves a function's index and GUID for one ADD, from the
// cache when the GUID is known and from the device otherwise.
func (r nodePodReader) deviceReader(ctx context.Context) deviceResolver {
	return func(deviceID, knownGUID string) (int, string, error) {
		return cachedPFDetail(ctx, r.guids, deviceID, knownGUID, connectiondetails.IBDeviceIndex, deviceReader(ctx))
	}
}

func (r nodePodReader) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := r.pods.Get(namespace, name)
	if err == nil {
		return pod, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	return r.api.GetPod(observability.WithLiveGetReason(ctx, "cache_miss"), namespace, name)
}

// nodePodWriter publishes through the API server and reads through the cache
// until one of its writes is rejected on its resourceVersion precondition.
// That rejection is evidence the copy the write was built from was already
// stale, and re-reading the same cached copy could only reproduce it, so every
// read after one is live.
type nodePodWriter struct {
	reader nodePodReader
	stale  bool
}

func (w *nodePodWriter) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if w.stale {
		return w.reader.api.GetPod(observability.WithLiveGetReason(ctx, "stale_write"), namespace, name)
	}
	return w.reader.GetPod(ctx, namespace, name)
}

// GetPodAuthoritative bypasses the cache outright. A caller reaches for this
// when a cached answer would be indistinguishable from a lost update.
func (w *nodePodWriter) GetPodAuthoritative(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return w.reader.api.GetPod(observability.WithLiveGetReason(ctx, "current_confirmation"), namespace, name)
}

func (w *nodePodWriter) PatchAnnotation(ctx context.Context, namespace, name, value,
	resourceVersion string, annotationsPresent bool,
) error {
	err := w.reader.api.PatchAnnotation(ctx, namespace, name, value, resourceVersion, annotationsPresent)
	// Only a rejected precondition says the copy this write was built from was
	// stale. A forbidden write, a validation error or a lost connection say
	// nothing about the cache, and escalating on those would send retries to
	// the API server for reasons re-reading cannot fix.
	if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
		w.stale = true
	}
	return err
}

// recheckingReader paces a readiness wait's reads. It answers from the cache
// but refuses to trust it indefinitely, and it refuses to ask the API server
// more than once per interval whatever the cache says. One is built per
// request, so the interval applies to each waiting attachment rather than to
// the daemon as a whole.
//
// The second rule is what keeps a missing Pod cheap. A cache miss has to fall
// back, or a Pod that has not reached this informer yet would be declared gone
// after five polls. But a Pod the cache never learns about would then be read
// live on every poll, five times a second for the whole readiness budget,
// which is the load this daemon exists to remove. Between checks the last
// authoritative answer stands, including a negative one.
type recheckingReader struct {
	reader   nodePodReader
	interval time.Duration
	next     time.Time
	pod      *corev1.Pod
	err      error
	asked    bool
}

func newRecheckingReader(reader nodePodReader, interval time.Duration) *recheckingReader {
	return &recheckingReader{reader: reader, interval: interval, next: time.Now().Add(interval)}
}

func (r *recheckingReader) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	now := time.Now()
	reason := "periodic_recheck"
	if !now.After(r.next) {
		reason = "cache_miss"
		pod, err := r.reader.pods.Get(namespace, name)
		switch {
		case err == nil:
			return pod, nil
		case !apierrors.IsNotFound(err):
			return nil, err
		case r.asked:
			return r.remembered()
		}
	}
	r.next = now.Add(r.interval)
	r.pod, r.err = r.reader.api.GetPod(observability.WithLiveGetReason(ctx, reason), namespace, name)
	r.asked = true
	return r.remembered()
}

func (r *recheckingReader) remembered() (*corev1.Pod, error) {
	if r.pod == nil {
		return nil, r.err
	}
	return r.pod.DeepCopy(), r.err
}

// daemonPublisher publishes this delegate and waits for the controller using
// node-local state. It is built per request so the readiness wait inherits
// that request's cancellation and the write path's staleness belongs to one
// caller rather than to every caller the daemon is serving.
func daemonPublisher(ctx context.Context, reader nodePodReader,
	readDevice deviceResolver,
) publisher {
	return func(args *skel.CmdArgs, netConf *localtypes.NetConf) error {
		published, err := publishRecorded(ctx, &nodePodWriter{reader: reader}, args, netConf, readDevice)
		if err != nil {
			return err
		}
		return awaitConnectionReady(ctx, newRecheckingReader(reader, cacheRecheckInterval),
			args, netConf, published)
	}
}

func daemonWithdrawer(ctx context.Context, reader nodePodReader) publisher {
	return func(args *skel.CmdArgs, netConf *localtypes.NetConf) error {
		return removePodConnectionDetailsExpecting(ctx, &nodePodWriter{reader: reader}, args,
			netConf.DeviceID, netConf.PodUID, expectedMembers(netConf))
	}
}

// emptyResult keeps DEL and CHECK on the transport's one-result-or-one-error
// contract. CNI defines no success payload for either command.
func emptyResult() json.RawMessage { return json.RawMessage(`{}`) }

// cniHandler serves one command per request from shared node state.
func cniHandler(reader nodePodReader) daemon.Handler {
	return func(ctx context.Context, request *daemon.Request) (*daemon.Response, error) {
		args := &skel.CmdArgs{
			ContainerID: request.ContainerID,
			Netns:       request.Netns,
			IfName:      request.IfName,
			Args:        request.Args,
			Path:        request.Path,
			StdinData:   request.StdinData,
		}
		observeCommandIdentity(ctx, args)
		namespace, name, _ := parsePodIdentity(args.Args)
		started := time.Now()
		klog.InfoS("cni command received", "command", request.Command, "namespace", namespace,
			"pod", name, "interface", args.IfName, "container", request.ContainerID)
		result, err := serveCommand(ctx, reader, request.Command, args)
		if err != nil {
			klog.ErrorS(err, "cni command failed", "command", request.Command, "namespace", namespace,
				"pod", name, "interface", args.IfName, "container", request.ContainerID,
				"elapsed", time.Since(started))
			return daemon.Failure(err), nil
		}
		klog.InfoS("cni command served", "command", request.Command, "namespace", namespace,
			"pod", name, "interface", args.IfName, "container", request.ContainerID,
			"elapsed", time.Since(started))
		return &daemon.Response{Result: result}, nil
	}
}

func serveCommand(ctx context.Context, reader nodePodReader, command string,
	args *skel.CmdArgs,
) (json.RawMessage, error) {
	switch command {
	case daemon.AddCommand:
		result, version, err := addNetwork(ctx, args,
			invocation{kube: daemonPublisher(ctx, reader, reader.deviceReader(ctx))})
		if err != nil {
			return nil, err
		}
		// Render here rather than in the caller: the CNI version is part of
		// the configuration this request carried, not of the plugin process
		// that forwarded it.
		versioned, err := result.GetAsVersion(version)
		if err != nil {
			return nil, err
		}
		return json.Marshal(versioned)
	case daemon.DelCommand:
		if err := delNetwork(ctx, args, invocation{kube: daemonWithdrawer(ctx, reader)}); err != nil {
			return nil, err
		}
		return emptyResult(), nil
	case daemon.CheckCommand:
		return emptyResult(), nil
	}
	return nil, fmt.Errorf("daemon does not serve command %q", command)
}

// discoveredSocketPath is where the plugin looks for a node daemon when the
// network definition names none. A variable so tests can stand a daemon there.
var discoveredSocketPath = daemon.DefaultSocketPath

// daemonRoute reports which node daemon serves this attachment, how long the
// caller may wait for it, and whether the socket was discovered rather than
// named by the definition. An unreadable configuration routes nowhere, so ADD
// still reports the parse failure in its own words and DEL stays lenient,
// exactly as they behave when no daemon is configured at all.
func daemonRoute(stdinData []byte) (socket string, timeout time.Duration, discovered bool) {
	netConf, err := config.LoadConf(stdinData)
	if err != nil {
		return "", 0, false
	}
	// This process is the one the runtime execs, in either mode, so it is the
	// one whose output would otherwise be discarded. Set its log up here,
	// before deciding whether to forward, so a failure to reach the daemon is
	// still recorded somewhere.
	setupPluginLogging(netConf)
	socket = netConf.DaemonSocketPath
	if socket == "" {
		// Discovered, not configured. daemonSocketPath cannot be the only way
		// in: network definitions are generated by tenant onboarding in the
		// environments this ships to, so a per-definition field would leave the
		// daemon unreachable there. Only an attachment that takes the handshake
		// looks, though. Every other configuration is served by this process
		// exactly as it was before there was a daemon: the daemon does not exec
		// delegates such as IPAM, and a network that never enabled the feature
		// must not start failing because a DaemonSet rolled out. An absent
		// socket likewise means the command runs in this process.
		if !netConf.AwaitsReadiness() {
			return "", 0, false
		}
		socket = discoveredSocketPath
		if _, err := os.Stat(socket); err != nil {
			// Worth saying out loud: in-process, every readiness poll is a
			// live API read, which is the load the node cache exists to
			// remove.
			klog.InfoS("no node daemon on this socket, serving in-process",
				"socket", socket, "readinessPollsTheAPIServer", true)
			return "", 0, false
		}
		discovered = true
	}
	timeout = daemonCallOverhead
	if netConf.AwaitsReadiness() {
		// The call must outlast the wait it is forwarding, or the daemon is cut
		// off before the attachment can become ready.
		timeout += netConf.ReadinessTimeout()
	}
	return socket, timeout, discovered
}

// daemonUnreachable is true when nothing answered on the socket at all: the
// file exists but no daemon listens (refused), or it vanished between the
// discovery stat and the connect. A daemon that answered, or one the caller
// timed out on, may have done the work; those errors are never a fallback.
func daemonUnreachable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)
}

// forwardCommand hands this invocation to the node daemon and returns the
// result bytes it produced. Nothing about the Pod is read in this process.
func forwardCommand(parent context.Context, args *skel.CmdArgs, command, socket string,
	timeout time.Duration,
) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := daemon.NewClient(socket)
	defer client.Close()
	response, err := client.Call(ctx, daemon.Request{
		Command:     command,
		ContainerID: args.ContainerID,
		Netns:       args.Netns,
		IfName:      args.IfName,
		Args:        args.Args,
		Path:        args.Path,
		StdinData:   args.StdinData,
	})
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	return response.Result, nil
}

// checkConfiguredSocket vets a daemonSocketPath named by a network definition
// before the plugin dials it as root: an absolute path, reached without a
// symbolic link, to a Unix socket owned by the plugin's own user. A tenant
// who can write a definition cannot point the plugin at an arbitrary socket
// and receive the CNI request. A missing socket is left to the dial, so an
// unreachable configured daemon is reported as before.
func checkConfiguredSocket(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("daemonSocketPath %q is not an absolute path", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemonSocketPath %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("daemonSocketPath %q is a symbolic link; name the socket itself", path)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("daemonSocketPath %q is not a Unix socket", path)
	}
	euid := os.Geteuid()
	if owner, ok := fileOwner(info); ok && (euid < 0 || euid > math.MaxUint32 || owner != uint32(euid)) {
		return fmt.Errorf("daemonSocketPath %q is owned by uid %d, not by the plugin's user %d", path, owner, euid)
	}
	return nil
}

// fileOwner is the uid that owns a file, when the platform reports one.
func fileOwner(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// prepareSocketDir makes the runtime directory this daemon owns. Mkdir applies
// the umask and a directory inherited from an earlier run may be too permissive
// for the server's own check, so the mode is always restated.
func prepareSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	if err := os.Mkdir(dir, daemonDirectoryMode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return os.Chmod(dir, daemonDirectoryMode)
}

func daemonClientset(kubeconfig string) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, err
	}
	cfg.Host = connectiondetails.NormalizeAPIServerHost(cfg.Host)
	return kubernetes.NewForConfig(cfg)
}

// runDaemon serves commands for one node until ctx ends. The cache is
// synchronized before the socket accepts anything, so no request is ever
// answered from a cache that has not yet listed the node's Pods. The GUID
// cache starts empty: every function is read on its first ADD after a start,
// and only then remembered.
func runDaemon(ctx context.Context, opt daemonOptions, kube kubernetes.Interface) error {
	if err := opt.validate(); err != nil {
		return err
	}
	setupDaemonLogging(opt)
	refresh, err := installPlugin(opt)
	if err != nil {
		return err
	}
	pods, err := podcache.New(kube, opt.node)
	if err != nil {
		return err
	}
	if err := prepareSocketDir(opt.socket); err != nil {
		return err
	}
	reader := nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(kube), guids: guidcache.New()}
	var metrics *observability.Metrics
	if opt.metricsBindAddress != "" {
		metrics = observability.NewMetrics()
	}
	server, err := daemon.NewServer(opt.socket, opt.requestTimeout, opt.maxInFlight, cniHandler(reader), daemon.WithMetrics(metrics))
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	runCtx, cancel := context.WithCancel(ctx)
	cacheDone := make(chan error, 1)
	go func() { cacheDone <- pods.Run(runCtx) }()
	defer func() { cancel(); <-cacheDone }()
	if refresh != nil {
		refreshDone := make(chan struct{})
		go func() { defer close(refreshDone); refresh(runCtx) }()
		defer func() { cancel(); <-refreshDone }()
	}
	syncCtx, stopSync := context.WithTimeout(runCtx, opt.syncTimeout)
	synced := pods.WaitForSync(syncCtx)
	stopSync()
	if !synced {
		return errors.New("node Pod cache did not complete its initial synchronization")
	}
	endpoint, err := startMetricsEndpoint(opt.metricsBindAddress, metrics)
	if err != nil {
		return fmt.Errorf("cannot start metrics listener: %w", err)
	}
	defer func() { _ = endpoint.Close() }()
	klog.InfoS("ib-sriov-cni daemon serving", "version", version, "commit", commit,
		"node", opt.node, "socket", opt.socket, "maxInFlight", opt.maxInFlight)
	err = server.Serve(runCtx)
	klog.InfoS("ib-sriov-cni daemon stopping", "node", opt.node, "socket", opt.socket)
	return err
}

func registerDaemonFlags(flags *flag.FlagSet) *daemonOptions {
	opt := &daemonOptions{}
	flags.BoolVar(&opt.enabled, "daemon", false,
		"serve CNI commands for this node from a shared Pod cache instead of running one command")
	flags.StringVar(&opt.node, "node-name", os.Getenv("NODE_NAME"),
		"node whose Pods are cached; no other node is ever watched")
	flags.StringVar(&opt.socket, "socket", daemon.DefaultSocketPath,
		"socket to serve, in a runtime directory this daemon owns")
	flags.StringVar(&opt.kubeconfig, "kubeconfig", "",
		"explicit kubeconfig; empty uses in-cluster credentials")
	flags.DurationVar(&opt.syncTimeout, "sync-timeout", daemonSyncTimeout,
		"limit on the initial Pod cache synchronization")
	flags.DurationVar(&opt.requestTimeout, "request-timeout", daemonRequestCeiling,
		"ceiling on one request; the caller's own deadline is the budget")
	flags.IntVar(&opt.maxInFlight, "max-in-flight", daemonMaxInFlight,
		"concurrent requests; callers beyond this are refused immediately")
	flags.IntVar(&opt.logLevel, "log-level", 0,
		"klog verbosity; 6 and above logs every request this daemon makes to the API server")
	flags.StringVar(&opt.metricsBindAddress, "metrics-bind-address", "",
		"optional Prometheus listen address, e.g. 127.0.0.1:9808; empty disables metrics; "+
			"non-loopback addresses require operator-managed access restrictions")
	flags.StringVar(&opt.logFile, "log-file", "",
		"also write this daemon's log to a rotated node-local file, so it can be read on the "+
			"node beside the plugin's own log; stdout is kept either way")
	flags.IntVar(&opt.logFileMaxSize, "log-file-max-size", defaultLogFileMaxSize,
		"megabytes before the daemon's log file is rotated")
	flags.IntVar(&opt.logFileMaxBackups, "log-file-max-backups", defaultLogFileMaxBackups,
		"rotated daemon log files to keep")
	flags.IntVar(&opt.logFileMaxAge, "log-file-max-age", defaultLogFileMaxAge,
		"days a rotated daemon log file is kept")
	flags.StringVar(&opt.installBinDir, "install-cni-bin-dir", "",
		"before serving, install this executable as ib-sriov in this host CNI binary directory; "+
			"empty installs nothing")
	flags.StringVar(&opt.cniConfDir, "cni-conf-dir", "/host/etc/cni/net.d",
		"host CNI configuration directory the node kubeconfig is written under")
	flags.BoolVar(&opt.genKubeconfig, "gen-kubeconfig", false,
		"write the node-local kubeconfig from this Pod's service account and refresh it while serving")
	return opt
}

func daemonExitCode(opt daemonOptions) int {
	defer klog.Flush()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Validate before anything reaches for credentials. A misconfigured daemon
	// should say what is wrong with its own flags, not report whatever the
	// client library complains about first.
	err := opt.validate()
	if err == nil {
		err = applyLogVerbosity(opt.logLevel)
	}
	if err == nil {
		var kube kubernetes.Interface
		if kube, err = daemonClientset(opt.kubeconfig); err == nil {
			err = runDaemon(ctx, opt, kube)
		}
	}
	if err != nil {
		klog.ErrorS(err, "ib-sriov-cni daemon exited")
		return 1
	}
	return 0
}
