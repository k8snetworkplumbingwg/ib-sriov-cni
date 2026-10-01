package connectiondetails

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeNodeGUID(t *testing.T) {
	got, err := normalizeGUID("946d:ae03:0033:9498\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "94:6d:ae:03:00:33:94:98" {
		t.Fatalf("got %q", got)
	}
	if _, err := normalizeGUID("nonsense"); err == nil {
		t.Fatal("expected error for malformed node_guid")
	}
	if _, err := normalizeGUID("0000:0000:0000:0000"); err == nil {
		t.Fatal("expected error for all-zero placeholder node_guid")
	}
	if _, err := normalizeGUID("ffff:ffff:ffff:ffff"); err == nil {
		t.Fatal("expected error for all-ones placeholder node_guid")
	}
}

// testBDF / testRawGUID are the PF and its sysfs node_guid the fake exposes.
const (
	testBDF     = "0000:af:00.0"
	testRawGUID = "946d:ae03:0033:9498"
)

// sysfsFake is a minimal PCI sysfs tree plus the kernel's reaction to driver
// writes: a BDF written to <drivers>/<driver>/bind re-points the device's
// driver symlink at that driver, and a BDF written to <device>/driver/unbind
// removes it. Writes are recorded by resolved path so tests can assert the
// exact rebind sequence and the final binding, not just file contents.
type sysfsFake struct {
	devices, drivers string
	writes           []string
	// failBindTo makes bind writes to this driver fail without touching the
	// tree, like a driver that refuses the device.
	failBindTo string
}

// fakeSysfs installs a tree where testBDF is bound to vfio-pci and already
// exposes an InfiniBand device (the fake kernel "probes" instantly), so
// ReadPFNodeGUID's full bind/read/rebind flow can run.
func fakeSysfs(t *testing.T) *sysfsFake {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &sysfsFake{devices: filepath.Join(root, "devices"), drivers: filepath.Join(root, "drivers")}
	devDir := filepath.Join(f.devices, testBDF)
	for _, d := range []string{
		devDir,
		filepath.Join(devDir, "infiniband", "mlx5_0"),
		filepath.Join(f.drivers, vfioPCIDriver),
		filepath.Join(f.drivers, mlx5CoreDriver),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(f.drivers, vfioPCIDriver), filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(devDir, "driver_override"),
		filepath.Join(f.drivers, vfioPCIDriver, "bind"),
		filepath.Join(f.drivers, vfioPCIDriver, "unbind"),
		filepath.Join(f.drivers, mlx5CoreDriver, "bind"),
		filepath.Join(f.drivers, mlx5CoreDriver, "unbind"),
	} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(devDir, "infiniband", "mlx5_0", "node_guid"), []byte(testRawGUID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldDevices, oldDrivers, oldWrite := sysBusPCIDevices, sysBusPCIDrivers, sysfsWriteFile
	sysBusPCIDevices, sysBusPCIDrivers, sysfsWriteFile = f.devices, f.drivers, f.write
	t.Cleanup(func() { sysBusPCIDevices, sysBusPCIDrivers, sysfsWriteFile = oldDevices, oldDrivers, oldWrite })
	return f
}

func (f *sysfsFake) write(path string, data []byte, perm os.FileMode) error {
	parent, base := filepath.Dir(path), filepath.Base(path)
	// An unbind reaches the current driver through the device's symlink;
	// resolve it so the write is attributed to that driver directory.
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	driverDir := strings.HasPrefix(parent, f.drivers+string(filepath.Separator))
	if base == "bind" && driverDir && filepath.Base(parent) == f.failBindTo {
		return fmt.Errorf("simulated bind failure for %s", f.failBindTo)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	f.writes = append(f.writes, filepath.Join(parent, base))
	link := filepath.Join(f.devices, strings.TrimSpace(string(data)), "driver")
	switch {
	case base == "bind" && driverDir:
		_ = os.Remove(link)
		return os.Symlink(parent, link)
	case base == "unbind" && driverDir:
		return os.Remove(link)
	}
	return nil
}

// rebinds lists the recorded driver bind/unbind writes as "<driver>/<op>".
func (f *sysfsFake) rebinds() []string {
	var out []string
	for _, w := range f.writes {
		if op := filepath.Base(w); op == "bind" || op == "unbind" {
			out = append(out, filepath.Base(filepath.Dir(w))+"/"+op)
		}
	}
	return out
}

// expect asserts the device's final driver ("" when unbound), the exact
// rebind sequence, and that no driver_override was left behind.
func (f *sysfsFake) expect(t *testing.T, driver string, rebinds ...string) {
	t.Helper()
	got, err := getPCIDeviceDriver(testBDF)
	if err != nil || got != driver {
		t.Fatalf("device bound to %q (err %v), want %q", got, err, driver)
	}
	if seq := f.rebinds(); strings.Join(seq, " ") != strings.Join(rebinds, " ") {
		t.Fatalf("rebind sequence %v, want %v", seq, rebinds)
	}
	assertOverrideCleared(t)
}

func assertOverrideCleared(t *testing.T) {
	t.Helper()
	override, err := os.ReadFile(filepath.Join(sysBusPCIDevices, testBDF, "driver_override"))
	if err != nil || strings.TrimSpace(string(override)) != "" {
		t.Fatalf("driver_override=%q err=%v; it must be cleared on every exit", override, err)
	}
}

func shortenPollTimeout(t *testing.T) {
	t.Helper()
	oldTimeout := ibDevicePollTimeout
	ibDevicePollTimeout = 20 * time.Millisecond
	t.Cleanup(func() { ibDevicePollTimeout = oldTimeout })
}

func TestReadPFNodeGUID(t *testing.T) {
	f := fakeSysfs(t)
	guid, err := ReadPFNodeGUID(testBDF)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guid != "94:6d:ae:03:00:33:94:98" {
		t.Fatalf("got %q", guid)
	}
	// The GUID must come through the rebind protocol and leave the PF back on
	// vfio-pci: unbind, bind mlx5_core, read, unbind, bind vfio-pci.
	f.expect(t, vfioPCIDriver, "vfio-pci/unbind", "mlx5_core/bind", "mlx5_core/unbind", "vfio-pci/bind")
}

func TestReadPFNodeGUIDRestoresAfterFailedMlx5Bind(t *testing.T) {
	f := fakeSysfs(t)
	// No mlx5_core driver directory: the bind write fails after the PF was
	// already unbound from vfio-pci, the stranded state the restore must cover.
	if err := os.RemoveAll(filepath.Join(sysBusPCIDrivers, mlx5CoreDriver)); err != nil {
		t.Fatal(err)
	}
	_, err := ReadPFNodeGUID(testBDF)
	if err == nil || !strings.Contains(err.Error(), "failed to bind PCI device") {
		t.Fatalf("expected mlx5_core bind failure, got %v", err)
	}
	f.expect(t, vfioPCIDriver, "vfio-pci/unbind", "vfio-pci/bind")
}

func TestReadPFNodeGUIDRestoresAfterPollTimeout(t *testing.T) {
	f := fakeSysfs(t)
	shortenPollTimeout(t)
	// mlx5_core never registers an InfiniBand device for the PF, so the PF is
	// left on mlx5_core until the restore runs.
	if err := os.RemoveAll(filepath.Join(sysBusPCIDevices, testBDF, "infiniband")); err != nil {
		t.Fatal(err)
	}
	_, err := ReadPFNodeGUID(testBDF)
	if err == nil || !strings.Contains(err.Error(), "not readable after") {
		t.Fatalf("expected poll timeout, got %v", err)
	}
	f.expect(t, vfioPCIDriver, "vfio-pci/unbind", "mlx5_core/bind", "mlx5_core/unbind", "vfio-pci/bind")
}

func TestReadPFNodeGUIDReportsBothReadAndRestoreFailure(t *testing.T) {
	f := fakeSysfs(t)
	shortenPollTimeout(t)
	if err := os.RemoveAll(filepath.Join(sysBusPCIDevices, testBDF, "infiniband")); err != nil {
		t.Fatal(err)
	}
	f.failBindTo = vfioPCIDriver
	_, err := ReadPFNodeGUID(testBDF)
	if err == nil || !strings.Contains(err.Error(), "not readable after") ||
		!strings.Contains(err.Error(), "additionally failed to bind PCI device") {
		t.Fatalf("expected the read error and the restore error together, got %v", err)
	}
	// The PF is genuinely stranded without a driver; the caller must know.
	f.expect(t, "", "vfio-pci/unbind", "mlx5_core/bind", "mlx5_core/unbind")
}

func TestReadPFNodeGUIDFailsWhenRestoreFails(t *testing.T) {
	f := fakeSysfs(t)
	f.failBindTo = vfioPCIDriver
	guid, err := ReadPFNodeGUID(testBDF)
	if err == nil || !strings.Contains(err.Error(), "back to "+vfioPCIDriver) || guid != "" {
		t.Fatalf("a successful read must not be returned when the PF is left off vfio-pci: guid=%q err=%v", guid, err)
	}
	f.expect(t, "", "vfio-pci/unbind", "mlx5_core/bind", "mlx5_core/unbind")
}

func TestBindPCIDeviceDriverBindsUnboundDevice(t *testing.T) {
	f := fakeSysfs(t)
	// A PF left without any driver (interrupted earlier run) must bind
	// straight to vfio-pci without an unbind write.
	if err := os.Remove(filepath.Join(sysBusPCIDevices, testBDF, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := bindPCIDeviceDriver(testBDF, vfioPCIDriver); err != nil {
		t.Fatal(err)
	}
	f.expect(t, vfioPCIDriver, "vfio-pci/bind")
}

func TestReadPFNodeGUIDRejectsNonCanonicalDeviceID(t *testing.T) {
	fakeSysfs(t)
	// The joined path of the last value resolves to the real device, so only
	// up-front validation keeps a non-canonical ID away from the sysfs writes.
	for _, deviceID := range []string{"", "0000:03:00", "../" + testBDF, testBDF + "/../" + testBDF} {
		if _, err := ReadPFNodeGUID(deviceID); err == nil {
			t.Fatalf("ReadPFNodeGUID(%q) accepted a non-canonical device ID", deviceID)
		}
	}
	unbind, err := os.ReadFile(filepath.Join(sysBusPCIDrivers, vfioPCIDriver, "unbind"))
	if err != nil || len(unbind) != 0 {
		t.Fatalf("vfio-pci unbind written (%q, err=%v) for a rejected device ID", unbind, err)
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

func TestIBDeviceIndexFailsOnUnreadableClass(t *testing.T) {
	fakePCIDevices(t, map[string]string{
		"0000:05:00.0": "0x020700",
		"0000:af:00.0": "0x020700",
	})
	// An unreadable class attribute (here a directory) must not be skipped:
	// silently dropping a device would shift every later rank down by one.
	if err := os.MkdirAll(filepath.Join(sysBusPCIDevices, "0000:3b:00.0", "class"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := IBDeviceIndex("0000:af:00.0"); err == nil {
		t.Fatal("expected error when a PCI device class cannot be read")
	}
}

// A canceled request stops the wait for the InfiniBand device at once; the
// vfio-pci restore still runs, because it is what the cancellation must not skip.
func TestReadPFNodeGUIDStopsPollingWhenCanceledAndRestores(t *testing.T) {
	f := fakeSysfs(t)
	if err := os.Remove(filepath.Join(f.devices, testBDF, "infiniband", "mlx5_0", "node_guid")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	started := time.Now()
	_, err := ReadPFNodeGUIDObserved(ctx, testBDF)
	elapsed := time.Since(started)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want a cancellation error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("discovery kept polling for %s after cancellation", elapsed)
	}
	f.expect(t, vfioPCIDriver, "vfio-pci/unbind", "mlx5_core/bind", "mlx5_core/unbind", "vfio-pci/bind")
}

// Partitions identify members by port GUID, and on a multi-port or
// differently programmed HCA the node GUID is not the port GUID. The published
// value is the port GUID of port 1, read from GID 0; node_guid is only the
// fallback when the port has no GID table yet.
func TestReadPFGUIDPublishesThePortGUIDFromGID0(t *testing.T) {
	f := fakeSysfs(t)
	portDir := filepath.Join(f.devices, testBDF, "infiniband", "mlx5_0", "ports", "1", "gids")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// node_guid is testRawGUID (…9498); the port's GID 0 carries a different GUID (…9499).
	if err := os.WriteFile(filepath.Join(portDir, "0"), []byte("fe80:0000:0000:0000:946d:ae03:0033:9499\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	guid, err := ReadPFNodeGUID(testBDF)
	if err != nil {
		t.Fatal(err)
	}
	if guid != "94:6d:ae:03:00:33:94:99" {
		t.Fatalf("published %q, want the port GUID 94:6d:ae:03:00:33:94:99 from GID 0, not node_guid", guid)
	}
}

func TestReadPFGUIDFallsBackToNodeGUIDWithoutAGIDTable(t *testing.T) {
	fakeSysfs(t)
	guid, err := ReadPFNodeGUID(testBDF)
	if err != nil {
		t.Fatal(err)
	}
	if guid != "94:6d:ae:03:00:33:94:98" {
		t.Fatalf("published %q, want node_guid 94:6d:ae:03:00:33:94:98 when the port has no GID table", guid)
	}
}
