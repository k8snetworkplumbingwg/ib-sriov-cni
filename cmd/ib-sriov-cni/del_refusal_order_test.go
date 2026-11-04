package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

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

// A DEL whose deviceID is not a PCI address withdraws nothing and does not
// fail: its ADD was refused at the same validation, so nothing was published,
// and the value never names a sysfs path.
func TestDELWithAnInvalidDeviceIDWithdrawsNothingAndSucceeds(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	escaped := escapedSysfsEntry(t, "physfn", filepath.Join(utils.SysBusPci, testPF))
	f, _ := newAnnotationTest(t)
	args := sandboxArgs("sandbox-a")
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"` + escaped + `",` +
		`"ibKubernetesEnabled":true,"publishPfGuid":true}`)
	if err := delNetwork(t.Context(), args, standalone(withdrawer(f))); err != nil {
		t.Fatalf("DEL with a non-PCI deviceID failed: %v", err)
	}
	if f.patches != 0 {
		t.Fatal("DEL with a non-PCI deviceID patched the Pod")
	}
}
