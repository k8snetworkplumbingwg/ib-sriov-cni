package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
)

const discoveredPublishingConfig = `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0",` +
	`"ibKubernetesEnabled":true,"publishPfGuid":true}`

// staleSocketAt leaves a socket file with nobody listening, as a daemon that
// was killed leaves one behind: connecting to it is refused.
func staleSocketAt(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ib-sriov.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A discovered socket is a hint, not an order: when no daemon answers on it,
// the attachment is served in this process, exactly as it would be had the
// file been absent. Otherwise a daemon that died without cleaning up would
// fail every publishing ADD on the node until someone removed the file.
func TestADiscoveredSocketWithNoDaemonBehindItServesInProcess(t *testing.T) {
	discoveredSocketAt(t, staleSocketAt(t))
	served := false
	err := executeCNICommand(&skel.CmdArgs{ContainerID: "c", IfName: "ib1", StdinData: []byte(discoveredPublishingConfig)},
		daemon.AddCommand, func(context.Context) error { served = true; return nil })
	if err != nil || !served {
		t.Fatalf("discovered dead socket: err=%v servedInProcess=%v; want in-process service and no error", err, served)
	}
}

// A socket the definition names is the operator's decision. Its daemon being
// unreachable is reported, not papered over by running in this process.
func TestAConfiguredSocketWithNoDaemonBehindItFails(t *testing.T) {
	path := staleSocketAt(t)
	config := `{"cniVersion":"1.0.0","name":"n","type":"ib-sriov","deviceID":"0000:03:00.0",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true,"daemonSocketPath":"` + path + `"}`
	served := false
	err := executeCNICommand(&skel.CmdArgs{ContainerID: "c", IfName: "ib1", StdinData: []byte(config)},
		daemon.AddCommand, func(context.Context) error { served = true; return nil })
	if err == nil || served {
		t.Fatalf("configured dead socket: err=%v servedInProcess=%v; want an error and no in-process service", err, served)
	}
}

// Only an unreachable daemon triggers the fallback. A daemon that answered has
// done or refused the work; running it again here could duplicate it.
func TestADiscoveredDaemonThatAnswersWithAnErrorDoesNotFallBack(t *testing.T) {
	socket := startStubDaemon(t, func(context.Context, *daemon.Request) (*daemon.Response, error) {
		return nil, errors.New("daemon says no")
	})
	discoveredSocketAt(t, socket)
	served := false
	err := executeCNICommand(&skel.CmdArgs{ContainerID: "c", IfName: "ib1", StdinData: []byte(discoveredPublishingConfig)},
		daemon.AddCommand, func(context.Context) error { served = true; return nil })
	if err == nil || served {
		t.Fatalf("daemon error: err=%v servedInProcess=%v; want the daemon's error and no in-process service", err, served)
	}
}
