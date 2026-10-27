package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

func pluginLogRootAt(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	previous := pluginLogRoot
	pluginLogRoot = root
	t.Cleanup(func() { pluginLogRoot = previous })
	return root
}

// The network definition names the file; the plugin owns the directory. A
// namespaced definition author must not hold a root-owned append, rename and
// truncate primitive on an arbitrary host path, so a directory part in the
// value is dropped and only the base name is used.
func TestLogFileIsConfinedToThePluginLogDirectory(t *testing.T) {
	root := pluginLogRootAt(t)
	for name, tc := range map[string]struct {
		configured string
		want       string
		wantErr    bool
	}{
		"unset means no file":             {"", "", false},
		"bare name":                       {"ib-sriov-cni.log", filepath.Join(root, "ib-sriov-cni.log"), false},
		"absolute path keeps its name":    {"/var/log/ib-sriov-cni.log", filepath.Join(root, "ib-sriov-cni.log"), false},
		"traversal keeps only the name":   {"../../etc/passwd", filepath.Join(root, "passwd"), false},
		"the directory itself is no file": {".", "", true},
		"the parent is no file":           {"..", "", true},
		"a slash is no file":              {"/", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := pluginLogPath(tc.configured)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("pluginLogPath(%q) = %q, %v; want %q, error=%v", tc.configured, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// An existing path that is not a regular file is refused rather than followed,
// so a link planted under the directory cannot redirect the log elsewhere.
func TestLogFileRefusesAnythingButARegularFile(t *testing.T) {
	root := pluginLogRootAt(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked.log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linked.log", "dir.log"} {
		if got, err := pluginLogPath(name); err == nil {
			t.Fatalf("%s accepted as %q", name, got)
		}
	}
	if got, err := pluginLogPath("plain.log"); err != nil || got != filepath.Join(root, "plain.log") {
		t.Fatalf("an existing regular file was refused: %q %v", got, err)
	}
}

// End to end: the definition's value lands as a file under the plugin's
// directory, and a refused value leaves the process logging to stderr only.
func TestPluginLoggingWritesUnderThePluginLogDirectory(t *testing.T) {
	t.Cleanup(func() { klog.SetOutput(os.Stderr) })
	root := pluginLogRootAt(t)
	// A path this process could create, so a leak would leave a file there.
	elsewhere := filepath.Join(t.TempDir(), "plugin.log")
	conf := &localtypes.NetConf{}
	conf.LogFile = elsewhere
	setupPluginLogging(conf)
	klog.InfoS("published connection-details", "device", "0000:03:00.0")
	klog.Flush()
	data, err := os.ReadFile(filepath.Join(root, "plugin.log"))
	if err != nil {
		t.Fatalf("no file under the plugin log directory: %v", err)
	}
	if !strings.Contains(string(data), "0000:03:00.0") {
		t.Fatalf("log does not carry the record: %q", data)
	}
	if _, err := os.Stat(elsewhere); !os.IsNotExist(err) {
		t.Fatal("the configured path was opened")
	}
}

// The reference daemon manifest's --log-file. The plugin's directory must be
// apart from it: both files are host-backed, and a definition author who could
// name the daemon's file would rotate it out from under the daemon's open
// descriptor and lose its records.
const referenceDaemonLogFile = "/var/log/ib-sriov-cni/daemon.log"

func TestPluginLogsLiveApartFromTheDaemonLog(t *testing.T) {
	t.Cleanup(func() { klog.SetOutput(os.Stderr) })
	host := t.TempDir()
	previous := pluginLogRoot
	pluginLogRoot = filepath.Join(host, pluginLogDir)
	t.Cleanup(func() { pluginLogRoot = previous })
	daemonLog := filepath.Join(host, referenceDaemonLogFile)
	if err := os.MkdirAll(filepath.Dir(daemonLog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonLog, []byte("daemon record\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	conf := &localtypes.NetConf{}
	conf.LogFile = "/var/log/daemon.log" // resolves to the daemon's base name on purpose
	setupPluginLogging(conf)
	klog.InfoS("plugin record", "device", "0000:03:00.0")
	klog.Flush()

	if got, err := os.ReadFile(daemonLog); err != nil || string(got) != "daemon record\n" {
		t.Fatalf("the daemon's file was touched by a plugin logFile: %q %v", got, err)
	}
	if backups, _ := filepath.Glob(filepath.Join(filepath.Dir(daemonLog), "daemon-*")); len(backups) != 0 {
		t.Fatalf("the daemon's file was rotated: %v", backups)
	}
	got, err := os.ReadFile(filepath.Join(pluginLogRoot, "daemon.log"))
	if err != nil || !strings.Contains(string(got), "plugin record") {
		t.Fatalf("plugin record not under the plugin directory: %q %v", got, err)
	}
}

// The separation is a rule the daemon holds too: it refuses to put its own log
// inside the plugin's directory, where a definition could name it.
func TestDaemonRefusesALogFileInsideThePluginLogDirectory(t *testing.T) {
	base := daemonOptions{node: "n", socket: "/run/x.sock", syncTimeout: 1, requestTimeout: 1, maxInFlight: 1,
		cniConfDir: "/host/etc/cni/net.d"}
	inside := base
	inside.logFile = filepath.Join(pluginLogDir, "daemon.log")
	if err := inside.validate(); err == nil {
		t.Fatal("a daemon log file inside the plugin log directory was accepted")
	}
	beside := base
	beside.logFile = referenceDaemonLogFile
	if err := beside.validate(); err != nil {
		t.Fatalf("the reference daemon log file was refused: %v", err)
	}
	if base.validate() != nil {
		t.Fatal("no log file must stay valid")
	}
}

// The plugin's log directory is created by the plugin itself, for root only:
// its lines name Pods and namespaces, and the writer would otherwise create
// it world-readable on first use.
func TestPluginLogDirectoryIsCreatedForRootOnly(t *testing.T) {
	root := filepath.Join(pluginLogRootAt(t), "fresh")
	pluginLogRoot = root
	path, err := pluginLogPath("ib-sriov-cni.log")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("log directory not created before the writer is installed: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode %v, want a directory with 0700", info.Mode())
	}
	if filepath.Dir(path) != root {
		t.Fatalf("log path %q is not under the created directory %q", path, root)
	}
}

// A directory left behind by an earlier writer, or made by hand, may be
// world-readable; the plugin tightens it rather than inheriting it.
func TestPluginLogDirectoryInheritedModeIsTightened(t *testing.T) {
	root := filepath.Join(pluginLogRootAt(t), "inherited")
	// MkdirAll's mode is filtered by the umask, so set the inherited mode
	// explicitly and check it, or under umask 077 this test would start from
	// 0700 and prove nothing.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("inherited directory not at 0755 before the test: %v, %v", info.Mode().Perm(), err)
	}
	pluginLogRoot = root
	if _, err := pluginLogPath("ib-sriov-cni.log"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("inherited log directory left at %v, want 0700", info.Mode().Perm())
	}
}
