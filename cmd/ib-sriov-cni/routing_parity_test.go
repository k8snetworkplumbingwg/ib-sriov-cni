package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// discoveredSocketAt points discovery at a socket a test controls and restores
// the well-known path afterwards.
func discoveredSocketAt(t *testing.T, path string) {
	t.Helper()
	previous := discoveredSocketPath
	discoveredSocketPath = path
	t.Cleanup(func() { discoveredSocketPath = previous })
}

// emptySysfsAt points device discovery at a sysfs tree with no devices in it,
// so a test's PCI address exists on no host the test runs on.
func emptySysfsAt(t *testing.T) {
	t.Helper()
	previous := utils.SysBusPci
	utils.SysBusPci = t.TempDir()
	t.Cleanup(func() { utils.SysBusPci = previous })
}

const legacyIPAMConfig = `{"cniVersion":"1.0.0","name":"legacy","type":"ib-sriov","deviceID":"0000:03:00.0",` +
	`"ipam":{"type":"host-local","subnet":"10.0.0.0/24"}}`

// Installing the daemon must not change what happens to a network that never
// enabled the handshake. Discovery is for readiness-enabled attachments only;
// everything else runs in this process exactly as it did before the socket
// appeared, whether or not the daemon could serve it.
func TestDaemonRouteDiscoversOnlyForReadinessConfigurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ib-sriov.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close the placeholder socket: %v", err)
		}
	})
	discoveredSocketAt(t, path)
	readiness := `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true}`
	for name, tc := range map[string]struct {
		config     string
		wantSocket string
	}{
		"legacy ipam stays in-process":        {legacyIPAMConfig, ""},
		"legacy vf without ipam stays":        {`{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0"}`, ""},
		"legacy vfio without publish stays":   {`{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0","ibKubernetesEnabled":true}`, ""},
		"publish without ib-kubernetes stays": {`{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0","publishPfGuid":true}`, ""},
		"readiness attachment is discovered":  {readiness, path},
		"explicit socket is honored as configured": {
			`{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0","daemonSocketPath":"` + path + `"}`, path},
	} {
		t.Run(name, func(t *testing.T) {
			socket, timeout, discovered := daemonRoute([]byte(tc.config))
			if socket != tc.wantSocket {
				t.Fatalf("daemonRoute = %q, want %q", socket, tc.wantSocket)
			}
			if discovered != (name == "readiness attachment is discovered") {
				t.Fatalf("discovered = %v for %q", discovered, name)
			}
			if tc.wantSocket == "" && timeout != 0 {
				t.Fatalf("in-process route carries a daemon budget %v", timeout)
			}
		})
	}
}

// The composed check the routing table cannot make: a legacy IPAM network's
// ADD and DEL never reach a daemon that exists at the well-known path.
func TestLegacyIPAMCommandsNeverReachThePresentDaemon(t *testing.T) {
	// Nothing here may touch the host: sysfs is an empty fake and node-local
	// state lives in directories the test owns.
	emptySysfsAt(t)
	cacheDirAt(t)
	var served atomic.Int32
	socket := startStubDaemon(t, func(_ context.Context, request *daemon.Request) (*daemon.Response, error) {
		served.Add(1)
		return daemon.Failure(errors.New("the daemon must not see legacy traffic: " + request.Command)), nil
	})
	discoveredSocketAt(t, socket)
	// The stub's own configuration through the same socket does forward, so
	// the stub is known to be reachable before the legacy cases are judged.
	if err := cmdDel(&skel.CmdArgs{ContainerID: "probe", IfName: "ib1", StdinData: routedConfig(socket)}); err == nil {
		t.Fatal("probe through the daemon reported success against a failing stub")
	}
	if served.Load() != 1 {
		t.Fatalf("stub not reachable: served=%d", served.Load())
	}

	// DEL with no netns is complete in-process without touching the node.
	args := &skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: []byte(legacyIPAMConfig),
		Args: "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1;K8S_POD_UID=uid-1"}
	if err := cmdDel(args); err != nil {
		t.Fatalf("legacy DEL failed: %v", err)
	}
	// ADD runs in-process and stops at a sandbox path that does not exist,
	// before any lock or device is touched: the point is where it stopped,
	// not that it failed.
	sandbox := filepath.Join(t.TempDir(), "netns")
	err := cmdAdd(&skel.CmdArgs{ContainerID: "cid", IfName: "ib1", Netns: sandbox,
		StdinData: []byte(legacyIPAMConfig), Args: args.Args})
	if err == nil || strings.Contains(err.Error(), "legacy traffic") {
		t.Fatalf("legacy ADD reached the daemon or succeeded: %v", err)
	}
	if !strings.Contains(err.Error(), sandbox) {
		t.Fatalf("legacy ADD did not stop at the missing sandbox: %v", err)
	}
	if served.Load() != 1 {
		t.Fatalf("daemon served %d legacy commands, want 0", served.Load()-1)
	}
}

// A readiness-enabled attachment still forwards, so discovery keeps doing the
// one job it exists for.
func TestReadinessAttachmentsStillDiscoverThePresentDaemon(t *testing.T) {
	served := make(chan string, 1)
	socket := startStubDaemon(t, func(_ context.Context, request *daemon.Request) (*daemon.Response, error) {
		served <- request.Command
		return &daemon.Response{Result: emptyResult()}, nil
	})
	discoveredSocketAt(t, socket)
	conf := `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true,"readinessTimeoutSeconds":1}`
	if err := cmdDel(&skel.CmdArgs{ContainerID: "cid", IfName: "ib1", StdinData: []byte(conf)}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-served:
		if got != daemon.DelCommand {
			t.Fatalf("daemon served %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readiness attachment was not forwarded to the discovered daemon")
	}
}
