package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// rebindTestPF points the fake PF's driver symlink at another driver, or
// removes it when driver is empty.
func rebindTestPF(t *testing.T, driver string) {
	t.Helper()
	link := filepath.Join(utils.SysBusPci, testPF, "driver")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if driver == "" {
		return
	}
	if err := os.Symlink("/not-a-real-driver/"+driver, link); err != nil {
		t.Fatal(err)
	}
}

// An ADD the runtime killed during the GUID read leaves the PF on mlx5_core
// or without a driver, and leaves the marker the read wrote before touching
// it. The next ADD for it, whether its definition states vfioPciMode or leaves
// it to detection, must reach the publication path so the read can restore the
// driver, instead of failing the mode check for ever.
func TestADDOfAPublishingPFLeftOffVfioReachesPublication(t *testing.T) {
	for _, driver := range []string{"mlx5_core", ""} {
		for mode, stated := range map[string]string{"vfioPciMode stated": `,"vfioPciMode":true`, "vfioPciMode detected": ""} {
			t.Run("left on "+driver+", "+mode, func(t *testing.T) {
				cacheDirAt(t)
				vfioPFAt(t)
				rebindTestPF(t, driver)
				if err := markRebind(testPF); err != nil {
					t.Fatal(err)
				}
				published := false
				publish := func(*skel.CmdArgs, *localtypes.NetConf) error { published = true; return nil }
				args := sandboxArgs("sandbox-a")
				args.Netns = "/proc/self/ns/net"
				args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"` + testPF + `",` +
					`"ibKubernetesEnabled":true,"publishPfGuid":true` + stated + `}`)
				_, _, err := addNetwork(t.Context(), args, standalone(publish))
				if !published {
					t.Fatalf("the ADD never reached publication: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), "not bound to vfio-pci") {
					t.Fatalf("the mode check refused the recoverable PF: %v", err)
				}
			})
		}
	}
}

// A retry whose Pod retains the GUID skips the device, which is what keeps a
// retry cheap. But the device read is also the only thing that puts a PF an
// interrupted ADD left on mlx5_core or without a driver back on vfio-pci, so
// the retained GUID may stand in only while the PF is on vfio-pci.
func TestARetainedGUIDDoesNotStandInForAPFLeftOffVfio(t *testing.T) {
	for _, onVfio := range []bool{true, false} {
		t.Run(fmt.Sprintf("on vfio-pci %v", onVfio), func(t *testing.T) {
			previous := deviceAccess
			t.Cleanup(func() { deviceAccess = previous })
			reads := 0
			deviceAccess.onVfioPCI = func(string) (bool, error) { return onVfio, nil }
			deviceAccess.index = func(string) (int, error) { return 0, nil }
			deviceAccess.readGUID = func(context.Context, string) (string, error) {
				reads++
				return "11:22:33:44:55:66:77:88", nil
			}
			_, guid, err := readPFDetail(t.Context(), testPF, "90:e3:17:03:00:fb:91:8e")
			if err != nil {
				t.Fatal(err)
			}
			if onVfio && (reads != 0 || guid != "90:e3:17:03:00:fb:91:8e") {
				t.Fatalf("reads=%d guid=%s, want the retained GUID without a device read", reads, guid)
			}
			if !onVfio && (reads != 1 || guid != "11:22:33:44:55:66:77:88") {
				t.Fatalf("reads=%d guid=%s, want the device read and restored despite the retained GUID", reads, guid)
			}
		})
	}
}

// Publication is the expensive step: it writes the Pod and then waits for the
// controller, for up to the readiness budget. The checks that need no API
// server, the network namespace and the RDMA mode, run first, so an ADD that
// cannot complete anyway never publishes and never holds a slot in the wait.
func TestADDWithoutANetnsDoesNotPublish(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	published := false
	publish := func(*skel.CmdArgs, *localtypes.NetConf) error { published = true; return nil }
	args := sandboxArgs("sandbox-a")
	args.Netns = filepath.Join(t.TempDir(), "absent")
	_, _, err := addNetwork(t.Context(), args, standalone(publish))
	if err == nil || !strings.Contains(err.Error(), "netns") {
		t.Fatalf("ADD without a netns = %v, want the netns error", err)
	}
	if published {
		t.Fatal("the ADD published before finding that its netns does not exist")
	}
}

// A publishing PF found off vfio-pci without the marker was not left there by
// this plugin's read: it is some other configuration's function, a host-owned
// netdev among them. Binding it to vfio-pci would take it from its owner, so
// the ADD is refused as it always was, in either mode, and nothing is
// published.
func TestAPublishingPFOffVfioWithoutAMarkerIsRefused(t *testing.T) {
	for _, driver := range []string{"mlx5_core", ""} {
		for mode, stated := range map[string]string{"vfioPciMode stated": `,"vfioPciMode":true`, "vfioPciMode detected": ""} {
			t.Run("found on "+driver+", "+mode, func(t *testing.T) {
				cacheDirAt(t)
				vfioPFAt(t)
				rebindTestPF(t, driver)
				published := false
				publish := func(*skel.CmdArgs, *localtypes.NetConf) error { published = true; return nil }
				args := sandboxArgs("sandbox-a")
				args.Netns = "/proc/self/ns/net"
				args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"` + testPF + `",` +
					`"ibKubernetesEnabled":true,"publishPfGuid":true` + stated + `}`)
				_, _, err := addNetwork(t.Context(), args, standalone(publish))
				if err == nil || published {
					t.Fatalf("unmarked PF off vfio-pci: err=%v published=%v; want refusal before publication", err, published)
				}
				if stated != "" && !strings.Contains(err.Error(), "not bound to vfio-pci") {
					t.Fatalf("stated mode refused with %v, want the mode check's error", err)
				}
			})
		}
	}
}

// The read marks the function before it moves it and clears the mark once the
// function is back on vfio-pci. A read that ends with the function elsewhere,
// as a failed restore does, leaves the mark, which is what lets the next ADD
// tell an interrupted read from someone else's device.
func TestTheReadMarksTheRebindUntilThePFIsBackOnVfio(t *testing.T) {
	for name, tc := range map[string]struct {
		readErr    error
		backOnVfio bool
		marked     bool
	}{
		"restored":       {nil, true, false},
		"restore failed": {errors.New("failed to bind back"), false, true},
		"read failed":    {errors.New("no gid"), true, false},
	} {
		t.Run(name, func(t *testing.T) {
			cacheDirAt(t)
			previous := deviceAccess
			t.Cleanup(func() { deviceAccess = previous })
			deviceAccess.index = func(string) (int, error) { return 0, nil }
			deviceAccess.onVfioPCI = func(string) (bool, error) { return tc.backOnVfio, nil }
			markedDuringRead := false
			deviceAccess.readGUID = func(context.Context, string) (string, error) {
				markedDuringRead = rebindInterrupted(testPF)
				return "11:22:33:44:55:66:77:88", tc.readErr
			}
			_, _, err := readPFDetail(t.Context(), testPF, "")
			if (err != nil) != (tc.readErr != nil) {
				t.Fatalf("readPFDetail = %v, want failure %v", err, tc.readErr != nil)
			}
			if !markedDuringRead {
				t.Fatal("the function was not marked while the read had it off vfio-pci")
			}
			if rebindInterrupted(testPF) != tc.marked {
				t.Fatalf("marked after the read = %v, want %v", rebindInterrupted(testPF), tc.marked)
			}
		})
	}
}
