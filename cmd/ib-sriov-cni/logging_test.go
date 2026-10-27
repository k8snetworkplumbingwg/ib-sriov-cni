package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

func TestPluginLoggingWritesTheNodeFile(t *testing.T) {
	t.Cleanup(func() { klog.SetOutput(os.Stderr) })
	path := filepath.Join(pluginLogRootAt(t), "ib-sriov-cni.log")
	conf := &localtypes.NetConf{}
	conf.LogFile = "ib-sriov-cni.log"
	setupPluginLogging(conf)
	// The record an operator has no other way to see: on success the runtime
	// discards this process's stderr.
	klog.InfoS("published connection-details", "device", "0000:03:00.0")
	klog.Flush()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no node log written: %v", err)
	}
	if !strings.Contains(string(data), "0000:03:00.0") {
		t.Fatalf("log does not carry the record: %q", data)
	}
}

func TestPluginLoggingIsOptInAndBounded(t *testing.T) {
	// No file configured: nothing is redirected, so the daemon and an
	// unconfigured plugin keep their stderr.
	before := filepath.Join(t.TempDir(), "unused.log")
	setupPluginLogging(&localtypes.NetConf{})
	if _, err := os.Stat(before); !os.IsNotExist(err) {
		t.Fatal("logging was set up without being asked for")
	}
	for _, tc := range []struct{ given, want int }{
		{0, defaultLogFileMaxSize}, {-1, defaultLogFileMaxSize}, {7, 7}, {maxLogFileMaxSize + 1, maxLogFileMaxSize},
	} {
		if got := bounded(tc.given, defaultLogFileMaxSize, maxLogFileMaxSize); got != tc.want {
			t.Fatalf("bounded(%d) = %d, want %d", tc.given, got, tc.want)
		}
	}
}

// The rotation fields come from the network definition, which is tenant input
// to a root process, so each is reduced to its cap: a definition cannot defeat
// the disk-usage bound of the host log. Unset or negative values take the
// defaults; values within the caps are used as given.
func TestRotationLimitsAreCapped(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		size, backups, age, wSize, wBackups, wAge int
	}{
		{"above the caps", 1 << 30, 1 << 30, 1 << 30, maxLogFileMaxSize, maxLogFileMaxBackups, maxLogFileMaxAge},
		{"unset", 0, 0, 0, defaultLogFileMaxSize, defaultLogFileMaxBackups, defaultLogFileMaxAge},
		{"within the caps", 50, 2, 1, 50, 2, 1},
	} {
		size, backups, age := rotationLimits(tc.size, tc.backups, tc.age)
		if size != tc.wSize || backups != tc.wBackups || age != tc.wAge {
			t.Fatalf("%s: rotationLimits = %d/%d/%d, want %d/%d/%d", tc.name, size, backups, age, tc.wSize, tc.wBackups, tc.wAge)
		}
	}
}

func TestLogFileIsAcceptedFromTheNetworkConfiguration(t *testing.T) {
	// The field has to survive parsing, since the definition is where an
	// operator sets it.
	raw := `{"cniVersion":"0.3.1","name":"n","type":"ib-sriov",` +
		`"logFile":"/var/log/ib-sriov-cni.log","logLevel":"4","logFileMaxBackups":2}`
	var conf localtypes.NetConf
	if err := json.Unmarshal([]byte(raw), &conf); err != nil {
		t.Fatal(err)
	}
	if conf.LogFile != "/var/log/ib-sriov-cni.log" || conf.LogLevel != "4" || conf.LogFileMaxBackups != 2 {
		t.Fatalf("configuration not parsed: %+v", conf)
	}
}

// The daemon is a Pod, so stdout stays the primary channel; the file is the
// second copy an operator reads on the node beside the plugin's own log.
func TestDaemonLoggingWritesItsOwnNodeFile(t *testing.T) {
	t.Cleanup(func() { klog.SetOutput(os.Stderr) })
	path := filepath.Join(t.TempDir(), "daemon.log")
	setupDaemonLogging(daemonOptions{logFile: path, logFileMaxBackups: 2})
	klog.InfoS("cni command served", "command", "ADD", "elapsed", "5s")
	klog.Flush()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no daemon log written: %v", err)
	}
	if !strings.Contains(string(data), "cni command served") {
		t.Fatalf("daemon log does not carry the record: %q", data)
	}
}

func TestDaemonLoggingStaysOffWhenUnset(t *testing.T) {
	unused := filepath.Join(t.TempDir(), "daemon.log")
	setupDaemonLogging(daemonOptions{})
	if _, err := os.Stat(unused); !os.IsNotExist(err) {
		t.Fatal("daemon logging was set up without being asked for")
	}
}
