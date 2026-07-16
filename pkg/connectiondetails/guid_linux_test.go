package connectiondetails

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeNodeGUID(t *testing.T) {
	got, err := normalizeNodeGUID("946d:ae03:0033:9498\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "94:6d:ae:03:00:33:94:98" {
		t.Fatalf("got %q", got)
	}
	if _, err := normalizeNodeGUID("nonsense"); err == nil {
		t.Fatal("expected error for malformed node_guid")
	}
	if _, err := normalizeNodeGUID("0000:0000:0000:0000"); err == nil {
		t.Fatal("expected error for all-zero placeholder node_guid")
	}
	if _, err := normalizeNodeGUID("ffff:ffff:ffff:ffff"); err == nil {
		t.Fatal("expected error for all-ones placeholder node_guid")
	}
}

// testBDF / testRawGUID are the PF and its sysfs node_guid the fake exposes.
const (
	testBDF     = "0000:af:00.0"
	testRawGUID = "946d:ae03:0033:9498"
)

// fakeSysfs builds a minimal PCI sysfs tree where testBDF is bound to
// vfio-pci and already exposes an InfiniBand device (the fake kernel "probes"
// instantly), so ReadPFNodeGUID's full bind/read/rebind flow can run.
func fakeSysfs(t *testing.T) {
	t.Helper()
	bdf := testBDF
	nodeGUID := testRawGUID
	root := t.TempDir()
	devices := filepath.Join(root, "devices")
	drivers := filepath.Join(root, "drivers")
	devDir := filepath.Join(devices, bdf)
	for _, d := range []string{
		devDir,
		filepath.Join(devDir, "infiniband", "mlx5_0"),
		filepath.Join(drivers, vfioPCIDriver),
		filepath.Join(drivers, mlx5CoreDriver),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// device bound to vfio-pci
	if err := os.Symlink(filepath.Join(drivers, vfioPCIDriver), filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		filepath.Join(devDir, "driver_override"),
		filepath.Join(drivers, vfioPCIDriver, "bind"),
		filepath.Join(drivers, vfioPCIDriver, "unbind"),
		filepath.Join(drivers, mlx5CoreDriver, "bind"),
		filepath.Join(drivers, mlx5CoreDriver, "unbind"),
	} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(devDir, "infiniband", "mlx5_0", "node_guid"), []byte(nodeGUID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldDevices, oldDrivers := sysBusPCIDevices, sysBusPCIDrivers
	sysBusPCIDevices, sysBusPCIDrivers = devices, drivers
	t.Cleanup(func() { sysBusPCIDevices, sysBusPCIDrivers = oldDevices, oldDrivers })
}

func TestReadPFNodeGUID(t *testing.T) {
	bdf := testBDF
	fakeSysfs(t)
	guid, err := ReadPFNodeGUID(bdf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guid != "94:6d:ae:03:00:33:94:98" {
		t.Fatalf("got %q", guid)
	}

	// The GUID must have come through the rebind protocol: unbind from
	// vfio-pci, bind to mlx5_core, and no driver_override left behind.
	for f, want := range map[string]string{
		filepath.Join(sysBusPCIDrivers, vfioPCIDriver, "unbind"): bdf,
		filepath.Join(sysBusPCIDrivers, mlx5CoreDriver, "bind"):  bdf,
		filepath.Join(sysBusPCIDevices, bdf, "driver_override"):  "",
	} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(raw)); got != want {
			t.Fatalf("%s = %q, want %q", f, got, want)
		}
	}
}

func TestReadPFNodeGUIDRejectsNonVfio(t *testing.T) {
	fakeSysfs(t)
	// rebind the fake device to mlx5_core directly
	devDir := filepath.Join(sysBusPCIDevices, testBDF)
	if err := os.Remove(filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sysBusPCIDrivers, mlx5CoreDriver), filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPFNodeGUID(testBDF); err == nil {
		t.Fatal("expected error when device is not bound to vfio-pci")
	}
}

// The full rebind path (unbind from the current driver, bind to the target,
// clear the override) is what the deferred vfio-pci restore executes after a
// successful GUID read; prove the write sequence.
func TestBindPCIDeviceDriverRebinds(t *testing.T) {
	bdf := testBDF
	fakeSysfs(t)
	devDir := filepath.Join(sysBusPCIDevices, bdf)
	if err := os.Remove(filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(sysBusPCIDrivers, mlx5CoreDriver), filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}

	if err := bindPCIDeviceDriver(bdf, vfioPCIDriver); err != nil {
		t.Fatalf("bindPCIDeviceDriver: %v", err)
	}

	for f, want := range map[string]string{
		filepath.Join(sysBusPCIDrivers, mlx5CoreDriver, "unbind"): bdf,
		filepath.Join(sysBusPCIDrivers, vfioPCIDriver, "bind"):    bdf,
		filepath.Join(devDir, "driver_override"):                  "",
	} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(raw)); got != want {
			t.Fatalf("%s = %q, want %q", f, got, want)
		}
	}
}

// A device already on the target driver but carrying a stale driver_override
// (left by an interrupted prior run) must have that override cleared, so it
// can't hijack a future bind.
func TestBindPCIDeviceDriverClearsStaleOverride(t *testing.T) {
	bdf := testBDF
	fakeSysfs(t) // device bound to vfio-pci
	overridePath := filepath.Join(sysBusPCIDevices, bdf, "driver_override")
	if err := os.WriteFile(overridePath, []byte(mlx5CoreDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bindPCIDeviceDriver(bdf, vfioPCIDriver); err != nil {
		t.Fatalf("bindPCIDeviceDriver: %v", err)
	}
	raw, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "" {
		t.Fatalf("stale driver_override not cleared: %q", got)
	}
}

// fakePCIDevices builds a PCI devices tree with the given BDF->class map and
// points sysBusPCIDevices at it.
func fakePCIDevices(t *testing.T, classes map[string]string) {
	t.Helper()
	devices := t.TempDir()
	for bdf, class := range classes {
		devDir := filepath.Join(devices, bdf)
		if err := os.MkdirAll(devDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(devDir, "class"), []byte(class+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldDevices := sysBusPCIDevices
	sysBusPCIDevices = devices
	t.Cleanup(func() { sysBusPCIDevices = oldDevices })
}

func TestIBDeviceIndex(t *testing.T) {
	// Rank is by sorted BDF among InfiniBand controllers (class 0x0207xx)
	// only; the ethernet controller must not shift it.
	fakePCIDevices(t, map[string]string{
		"0000:af:00.0": "0x020700",
		"0000:05:00.0": "0x020700",
		"0000:c3:00.0": "0x020700",
		"0000:3b:00.0": "0x020000", // ethernet, ignored
	})
	for bdf, want := range map[string]int{
		"0000:05:00.0": 0,
		"0000:af:00.0": 1,
		"0000:c3:00.0": 2,
	} {
		got, err := IBDeviceIndex(bdf)
		if err != nil {
			t.Fatalf("IBDeviceIndex(%q) returned error: %v", bdf, err)
		}
		if got != want {
			t.Fatalf("IBDeviceIndex(%q) = %d, want %d", bdf, got, want)
		}
	}

	if _, err := IBDeviceIndex("0000:3b:00.0"); err == nil {
		t.Fatal("expected error for a non-InfiniBand device")
	}
	if _, err := IBDeviceIndex("0000:ff:00.0"); err == nil {
		t.Fatal("expected error for an unknown device")
	}
}
