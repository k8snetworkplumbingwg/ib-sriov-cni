package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
)

func observedPluginEnv(command string) []string {
	return []string{
		"CNI_COMMAND=" + command, "CNI_CONTAINERID=synthetic", "CNI_IFNAME=ib1",
		"CNI_NETNS=/not-a-real-netns", "CNI_PATH=/opt/cni/bin",
		"CNI_ARGS=K8S_POD_NAMESPACE=tenant;K8S_POD_NAME=a;K8S_POD_UID=uid-a",
	}
}

func TestProcessCallbackCompletionSummaries(t *testing.T) {
	for _, tc := range []struct {
		name, command, mode, result string
		forward                     bool
	}{
		{name: "standalone_add_error", command: "ADD", mode: "standalone", result: "error"},
		{name: "standalone_del", command: "DEL", mode: "standalone", result: "success"},
		{name: "standalone_check", command: "CHECK", mode: "standalone", result: "success"},
		{name: "failed_forward", command: "ADD", mode: "shim", result: "error", forward: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := map[string]string{
				"cniVersion": "1.0.0", "name": "net-a", "type": "ib-sriov", "token": "do-not-log-this-value",
			}
			if tc.forward {
				conf["daemonSocketPath"] = filepath.Join(t.TempDir(), "absent.sock")
			}
			data, err := json.Marshal(conf)
			if err != nil {
				t.Fatal(err)
			}
			plugin := startProcess(t, shippedBinary, observedPluginEnv(tc.command), string(data))
			awaitProcess(t, plugin)
			if (plugin.err != nil) != (tc.result == "error") {
				t.Fatalf("exit=%v stderr=%s", plugin.err, plugin.stderr.String())
			}
			log := plugin.stderr.String()
			for _, field := range []string{`mode="` + tc.mode + `"`, `command="` + tc.command + `"`,
				`result="` + tc.result + `"`, `pod="a"`, `podUID="uid-a"`, `interface="ib1"`, "elapsed="} {
				if !strings.Contains(log, field) {
					t.Fatalf("summary missing %s: %s", field, log)
				}
			}
			if strings.Count(log, "CNI request completed") != 1 || strings.Contains(log, conf["token"]) {
				t.Fatalf("missing/duplicate/unsafe summary: %s", log)
			}
			if tc.result == "error" {
				var cniErr cnitypes.Error
				if err := json.Unmarshal([]byte(plugin.stdout.String()), &cniErr); err != nil || cniErr.Code == 0 {
					t.Fatalf("stdout is not solely a CNI error: %s err=%v", plugin.stdout.String(), err)
				}
			} else if plugin.stdout.String() != "" {
				t.Fatalf("telemetry leaked to CNI stdout: %s", plugin.stdout.String())
			}
		})
	}
}

func awaitCompletionSummaries(t *testing.T, process *childProcess, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(process.stderr.String(), "CNI request completed") >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing %d completion summaries: %s", count, process.stderr.String())
}

func TestProcessDaemonMetricsDoNotReadAPIAndStopWithDaemon(t *testing.T) {
	api := newProcessAPI(t, false)
	socket, kubeconfig := processFiles(t, api.server.URL)
	process := startProcess(t, shippedBinary, nil, "", "--daemon", "--node-name=node-a",
		"--socket="+socket, "--kubeconfig="+kubeconfig, "--sync-timeout=5s", "--metrics-bind-address=127.0.0.1:0")
	awaitAPIStart(t, api, process)
	awaitServing(t, process)
	match := regexp.MustCompile(`daemon metrics serving.*address="([^"]+)"`).FindStringSubmatch(process.stderr.String())
	if len(match) != 2 {
		t.Fatalf("metrics listener not announced: %s", process.stderr.String())
	}
	address := match[1]
	for range 2 {
		args := &skel.CmdArgs{ContainerID: "synthetic", IfName: "ib1", StdinData: []byte(`{}`)}
		if _, err := forwardCommand(t.Context(), args, daemon.CheckCommand, socket, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	awaitCompletionSummaries(t, process, 2)
	before := api.counts()
	client := &http.Client{Timeout: 2 * time.Second}
	for range 3 {
		response, err := client.Get("http://" + address + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body),
			`ib_sriov_cni_request_duration_seconds_count{command="CHECK",result="success"} 2`) {
			t.Fatalf("scrape status=%d body=%s err=%v", response.StatusCode, body, err)
		}
	}
	if after := api.counts(); after != before || after.gets != 0 {
		t.Fatalf("scrapes caused API work: before=%+v after=%+v", before, after)
	}
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitProcess(t, process)
	if process.err != nil {
		t.Fatal(fmt.Errorf("daemon shutdown: %w: %s", process.err, process.stderr.String()))
	}
	if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("metrics listener outlived daemon")
	}
}
