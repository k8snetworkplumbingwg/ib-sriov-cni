package connectiondetails

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/daemon"
)

func TestDaemonShutdownWaitsForPFRestore(t *testing.T) {
	f := fakeSysfs(t)
	guidPath := filepath.Join(f.devices, testBDF, "infiniband", "mlx5_0", "node_guid")
	if err := os.Remove(guidPath); err != nil {
		t.Fatal(err)
	}
	bound := make(chan struct{})
	originalWrite := sysfsWriteFile
	sysfsWriteFile = func(path string, data []byte, mode os.FileMode) error {
		err := originalWrite(path, data, mode)
		if err == nil && path == filepath.Join(f.drivers, mlx5CoreDriver, "bind") {
			close(bound)
		}
		return err
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "daemon.sock")
	handlerDone := make(chan struct{})
	server, err := daemon.NewServer(socket, 20*time.Second, 1, func(ctx context.Context, _ *daemon.Request) (*daemon.Response, error) {
		defer close(handlerDone)
		_, err := ReadPFNodeGUIDObserved(ctx, testBDF)
		if err != nil {
			return daemon.Failure(err), nil
		}
		return &daemon.Response{Result: json.RawMessage(`{}`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()
	clientCtx, cancelClient := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelClient()
	client := daemon.NewClient(socket)
	defer client.Close()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		_, _ = client.Call(clientCtx, daemon.Request{Command: daemon.AddCommand, ContainerID: "cid", IfName: "ib1", StdinData: []byte(`{}`)})
	}()
	select {
	case <-bound:
	case <-time.After(3 * time.Second):
		t.Fatal("GUID read did not bind mlx5_core")
	}
	started := time.Now()
	stopServer() // what SIGTERM does to the daemon
	select {
	case err := <-serverDone:
		t.Logf("Serve returned after %s: %v", time.Since(started), err)
	case <-time.After(12 * time.Second):
		t.Fatal("Serve never returned")
	}
	current, err := getPCIDeviceDriver(testBDF)
	if err != nil {
		t.Fatal(err)
	}
	if current != vfioPCIDriver {
		t.Errorf("Serve returned before restoration: PF still bound to %q; daemon main now os.Exit's", current)
	}
	// The shipped daemon exits when Serve returns, so the restore must already have run by then.
	// Join the unfinished handler before fakeSysfs restores global variables.
	select {
	case <-handlerDone:
	case <-time.After(12 * time.Second):
		t.Fatal("GUID handler did not finish")
	}
	<-clientDone
}
