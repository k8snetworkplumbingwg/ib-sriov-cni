package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// A DEL the daemon will refuse, because the configuration it carries needs a
// delegate this process cannot exec, must be refused before anything is
// withdrawn: the published member is the controller's record of a binding
// that is still in place, and a refused teardown has not released it.
func TestDaemonDELRefusesADelegateConfigurationBeforeWithdrawing(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	args := sandboxArgs("sandbox-a")
	publishFrom(t, f, args, mine)
	patches := f.patches
	// The device was rebound to mlx5_core after ADD and the definition now
	// carries IPAM: teardown would exec a delegate, which the daemon cannot.
	rebindTestPF(t, "mlx5_core")
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"0000:03:00.0",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true,"ipam":{"type":"host-local","subnet":"10.0.0.0/24"}}`)
	err := delNetwork(t.Context(), args, invocation{kube: withdrawer(f)})
	if err == nil {
		t.Fatal("daemon DEL with an IPAM delegate was not refused")
	}
	if f.patches != patches {
		t.Fatal("the refused DEL withdrew the published member first")
	}
	checkEntry(t, f, mine, 1)
}

// rebindTestPF points the fake PF's driver symlink at another driver.
func rebindTestPF(t *testing.T, driver string) {
	t.Helper()
	link := filepath.Join(utils.SysBusPci, testPF, "driver")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/not-a-real-driver/"+driver, link); err != nil {
		t.Fatal(err)
	}
}

// ADD auto-detects a vfio-pci device and allows the definition's IPAM section,
// which is never executed for a device without a network interface. DEL
// applies the same detection to the configuration the runtime hands it, so a
// configuration the daemon accepted for ADD is not refused for DEL, which
// would leave the Pod terminating and the member published for ever.
func TestDaemonDELAppliesVFIODetectionBeforeTheDelegateRule(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	args := sandboxArgs("sandbox-a")
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"0000:03:00.0",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true,"ipam":{"type":"host-local","subnet":"10.0.0.0/24"}}`)
	publishFrom(t, f, args, mine)
	args.Netns = ""
	if err := delNetwork(t.Context(), args, invocation{kube: withdrawer(f)}); err != nil {
		t.Fatalf("daemon DEL of a vfio-pci device with an IPAM section was refused: %v", err)
	}
	if details := detailsOf(t, f); len(details) != 0 {
		t.Fatalf("the DEL did not withdraw the member: %+v", details)
	}
}

// escapedSysfsEntry creates a directory outside the fake sysfs root that a
// deviceID containing ".." would reach, and gives it the one entry named.
func escapedSysfsEntry(t *testing.T, entry, target string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(utils.SysBusPci), "escape")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, entry)); err != nil {
		t.Fatal(err)
	}
	return "../escape"
}

// The deviceID the runtime hands a DEL is validated before the VFIO detection
// builds a sysfs path from it: a value that is not a PCI address never reaches
// the filesystem, so it cannot select a driver link outside the device tree
// and change how the delegate rule is applied.
func TestDaemonDELValidatesTheDeviceIDBeforeVFIODetection(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	escaped := escapedSysfsEntry(t, "driver", "/not-a-real-driver/vfio-pci")
	f, _ := newAnnotationTest(t)
	args := sandboxArgs("sandbox-a")
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"` + escaped + `",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true,"ipam":{"type":"host-local","subnet":"10.0.0.0/24"}}`)
	err := delNetwork(t.Context(), args, invocation{kube: withdrawer(f)})
	if err == nil {
		t.Fatal("a DEL whose deviceID escapes the device tree was treated as a vfio-pci device and its IPAM delegate accepted")
	}
	if f.patches != 0 {
		t.Fatal("the refused DEL patched the Pod")
	}
}

// A DEL whose deviceID is not a PCI address withdraws nothing and does not
// fail: its ADD was refused at the same validation, so nothing was published.
func TestDaemonDELWithAnInvalidDeviceIDIsLenient(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	escaped := escapedSysfsEntry(t, "physfn", filepath.Join(utils.SysBusPci, testPF))
	f, _ := newAnnotationTest(t)
	args := sandboxArgs("sandbox-a")
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"` + escaped + `",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true}`)
	if err := delNetwork(t.Context(), args, invocation{kube: withdrawer(f)}); err != nil {
		t.Fatalf("DEL with a non-PCI deviceID failed: %v", err)
	}
	if f.patches != 0 {
		t.Fatal("DEL with a non-PCI deviceID patched the Pod")
	}
}
