package connectiondetails

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

const (
	mlx5CoreDriver = "mlx5_core"
	vfioPCIDriver  = "vfio-pci"

	// sysfsWriteMode is inert for existing sysfs attributes (the mode only
	// applies at file creation).
	sysfsWriteMode = 0o600

	// ibDevicePollInterval paces the wait for the InfiniBand device directory
	// to appear after binding the PF to mlx5_core.
	ibDevicePollInterval = 250 * time.Millisecond
)

// ibDevicePollTimeout bounds that wait; a variable so tests can shorten it.
var ibDevicePollTimeout = 10 * time.Second

// sysBusPCI* are variables so tests can point them at a fake sysfs tree.
var (
	sysBusPCIDevices = "/sys/bus/pci/devices"
	sysBusPCIDrivers = "/sys/bus/pci/drivers"
)

// sysfsWriteFile performs every sysfs write of this file. Tests replace it to
// mirror the kernel's reaction to bind/unbind writes in the fake tree.
var sysfsWriteFile = os.WriteFile

// getPCIDeviceDriver returns the name of the driver currently bound to the PCI
// device (e.g. "vfio-pci", "mlx5_core"), or "" if no driver is bound.
func getPCIDeviceDriver(deviceID string) (string, error) {
	driverTarget, err := os.Readlink(filepath.Join(sysBusPCIDevices, deviceID, "driver"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read driver for PCI device %s: %w", deviceID, err)
	}
	return filepath.Base(driverTarget), nil
}

// writePCIDeviceDriverOverride sets the PCI device's driver_override to driver,
// or clears it (restoring default driver matching) when driver is empty.
func writePCIDeviceDriverOverride(deviceID, driver string) error {
	overrideValue := []byte(driver)
	if driver == "" {
		overrideValue = []byte("\n")
	}
	overridePath := filepath.Join(sysBusPCIDevices, deviceID, "driver_override")
	if err := sysfsWriteFile(overridePath, overrideValue, sysfsWriteMode); err != nil {
		return fmt.Errorf("failed to write driver override for PCI device %s: %w", deviceID, err)
	}
	return nil
}

// bindPCIDeviceDriver rebinds deviceID to driver via driver_override.
// Idempotent: returns nil if already bound. Clears the override on both
// success and failure so the device is never left with a stale override.
func bindPCIDeviceDriver(deviceID, driver string) error {
	return bindPCIDeviceDriverObserved(context.Background(), deviceID, driver)
}

func bindPCIDeviceDriverObserved(ctx context.Context, deviceID, driver string) error {
	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return err
	}
	if currentDriver == driver {
		// Already on the target driver; still clear any stale driver_override a
		// prior interrupted run may have left, so it can't affect future binds.
		return writePCIDeviceDriverOverride(deviceID, "")
	}

	if err := writePCIDeviceDriverOverride(deviceID, driver); err != nil {
		return err
	}

	if currentDriver != "" {
		unbindPath := filepath.Join(sysBusPCIDevices, deviceID, "driver", "unbind")
		phase := "vfio_unbind"
		if currentDriver == mlx5CoreDriver {
			phase = "mlx5_unbind"
		}
		finish := observability.Detail(ctx, phase)
		err := sysfsWriteFile(unbindPath, []byte(deviceID), sysfsWriteMode)
		finish(err)
		if err != nil {
			_ = writePCIDeviceDriverOverride(deviceID, "")
			return fmt.Errorf("failed to unbind PCI device %s from driver %s: %w", deviceID, currentDriver, err)
		}
	}

	bindPath := filepath.Join(sysBusPCIDrivers, driver, "bind")
	phase := "mlx5_bind"
	if driver == vfioPCIDriver {
		phase = "vfio_bind"
	}
	finish := observability.Detail(ctx, phase)
	err = sysfsWriteFile(bindPath, []byte(deviceID), sysfsWriteMode)
	finish(err)
	if err != nil {
		_ = writePCIDeviceDriverOverride(deviceID, "")
		return fmt.Errorf("failed to bind PCI device %s to driver %s: %w", deviceID, driver, err)
	}

	return writePCIDeviceDriverOverride(deviceID, "")
}

// readPFGUIDSysfs reads the GUID of the first InfiniBand device under the
// PF's sysfs entry, polling until the device appears (driver probe is
// asynchronous after bind). The value is the port GUID of port 1, taken from
// GID 0: partitions identify their members by port GUID, and on a multi-port
// or differently programmed HCA that is not the node GUID. node_guid is the
// fallback for a device whose port exposes no GID table.
func readPFGUIDSysfs(ctx context.Context, deviceID string) (_ string, retErr error) {
	finish := observability.Detail(ctx, "guid_discovery")
	defer func() { finish(retErr) }()
	ibDir := filepath.Join(sysBusPCIDevices, deviceID, "infiniband")
	deadline := time.Now().Add(ibDevicePollTimeout)
	for {
		observability.DiscoveryPoll(ctx)
		entries, err := os.ReadDir(ibDir)
		switch {
		case err == nil && len(entries) > 0:
			devDir := filepath.Join(ibDir, entries[0].Name())
			if gid, gidErr := os.ReadFile(filepath.Clean(filepath.Join(devDir, "ports", "1", "gids", "0"))); gidErr == nil {
				return portGUIDFromGID(string(gid))
			}
			raw, readErr := os.ReadFile(filepath.Clean(filepath.Join(devDir, "node_guid")))
			if readErr == nil {
				// Once the IB device is registered, the GUID is the real value;
				// a malformed/placeholder read is returned as an error (logged,
				// not published) rather than polled — re-reading won't fix a bad
				// value, and the outer CNI ADD retry covers a true transient.
				return normalizeGUID(string(raw))
			}
			err = readErr
		case err == nil:
			// The infiniband directory exists but mlx5_core has not yet
			// registered an IB device under it (driver probe still in progress).
			err = fmt.Errorf("no InfiniBand device registered under %s yet", ibDir)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("InfiniBand device for PCI device %s not readable after %s: %w",
				deviceID, ibDevicePollTimeout, err)
		}
		// A canceled request stops waiting here, not after the deadline: the
		// caller's deferred vfio-pci restore runs next, and a daemon that is
		// shutting down waits for exactly that before it exits.
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("InfiniBand device for PCI device %s not read: %w (last: %v)", deviceID, ctx.Err(), err)
		case <-time.After(ibDevicePollInterval):
		}
	}
}

// A 64-bit GUID is 8 octets of 2 hex characters each once the sysfs
// colons are stripped.
const (
	guidOctets    = 8
	octetHexChars = 2
	guidHexChars  = guidOctets * octetHexChars
	// gidGroups and guidGroups are the colon-separated 16-bit groups in a sysfs GID and in the GUID it ends with.
	gidGroups  = 8
	guidGroups = 4
)

// normalizeGUID converts the sysfs node_guid format ("946d:ae03:0033:9498")
// to the colon-separated 8-octet form ib-kubernetes parses with net.ParseMAC
// ("94:6d:ae:03:00:33:94:98").
// portGUIDFromGID takes the port GUID out of a sysfs GID, the link-local
// address fe80::<port GUID> that GID 0 holds for an InfiniBand port.
func portGUIDFromGID(raw string) (string, error) {
	groups := strings.Split(strings.TrimSpace(raw), ":")
	if len(groups) != gidGroups {
		return "", fmt.Errorf("unexpected GID %q", strings.TrimSpace(raw))
	}
	return normalizeGUID(strings.Join(groups[gidGroups-guidGroups:], ":"))
}

func normalizeGUID(raw string) (string, error) {
	hexStr := strings.ReplaceAll(strings.TrimSpace(raw), ":", "")
	if len(hexStr) != guidHexChars {
		return "", fmt.Errorf("unexpected GUID %q", strings.TrimSpace(raw))
	}
	// Reject the uninitialized (all-zero) and all-ones sentinels: these read
	// as well-formed but are meaningless to publish to a consumer.
	if hexStr == strings.Repeat("0", guidHexChars) || strings.EqualFold(hexStr, strings.Repeat("f", guidHexChars)) {
		return "", fmt.Errorf("placeholder GUID %q", strings.TrimSpace(raw))
	}
	parts := make([]string, 0, guidOctets)
	for i := 0; i < guidHexChars; i += octetHexChars {
		parts = append(parts, hexStr[i:i+octetHexChars])
	}
	guid := strings.Join(parts, ":")
	if _, err := net.ParseMAC(guid); err != nil {
		return "", fmt.Errorf("%q does not parse as a GUID: %w", strings.TrimSpace(raw), err)
	}
	return guid, nil
}

// ReadPFNodeGUID reads the port GUID of a PF currently bound to vfio-pci by
// temporarily rebinding it to mlx5_core. The PF is rebound to vfio-pci on
// every path, including errors; a rebind failure is surfaced in the returned
// error so the caller never silently proceeds with the device off vfio-pci.
//
// Safe at CNI ADD time in PF VFIO mode: the device plugin has allocated the
// device but the VM has not started, so the vfio group is not yet in use.
func ReadPFNodeGUID(deviceID string) (guid string, retErr error) {
	return ReadPFNodeGUIDObserved(context.Background(), deviceID)
}

// ReadPFNodeGUIDObserved retains the same driver/restore contract and attaches
// diagnostic timings to the caller's request, when one is present.
func ReadPFNodeGUIDObserved(ctx context.Context, deviceID string) (guid string, retErr error) {
	if err := ValidateDeviceID(deviceID); err != nil {
		return "", err
	}
	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return "", err
	}
	if currentDriver != vfioPCIDriver {
		return "", fmt.Errorf("PCI device %s is not bound to %s, found %q", deviceID, vfioPCIDriver, currentDriver)
	}

	// Register the vfio-pci restore BEFORE binding to mlx5_core: if that bind
	// fails after the device is already unbound from vfio-pci, an early return
	// would otherwise leave the PF stranded with no driver. bindPCIDeviceDriver
	// is idempotent, so restoring after a failed bind is safe.
	defer func() {
		finish := observability.Detail(ctx, "vfio_restore")
		err := bindPCIDeviceDriverObserved(ctx, deviceID, vfioPCIDriver)
		finish(err)
		if err != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w; additionally failed to bind PCI device %s back to %s: %v",
					retErr, deviceID, vfioPCIDriver, err)
			} else {
				guid = ""
				retErr = fmt.Errorf("failed to bind PCI device %s back to %s: %w", deviceID, vfioPCIDriver, err)
			}
		}
	}()

	if err := bindPCIDeviceDriverObserved(ctx, deviceID, mlx5CoreDriver); err != nil {
		return "", fmt.Errorf("failed to bind PCI device %s to %s for GUID read: %w", deviceID, mlx5CoreDriver, err)
	}

	return readPFGUIDSysfs(ctx, deviceID)
}

// IBDeviceIndex returns deviceID's rank among the node's InfiniBand
// controllers (PCI class 0x0207xx) in sorted-BDF order - the same order
// lspci lists them and fabric managers number deviceInstance. Works
// regardless of driver binding (no rebind needed).
func IBDeviceIndex(deviceID string) (int, error) {
	entries, err := os.ReadDir(sysBusPCIDevices)
	if err != nil {
		return -1, err
	}
	var ibBDFs []string
	for _, e := range entries {
		classRaw, err := os.ReadFile(filepath.Join(sysBusPCIDevices, e.Name(), "class"))
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed while enumerating
			}
			return -1, fmt.Errorf("failed to read class of PCI device %s: %w", e.Name(), err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(classRaw)), "0x0207") {
			ibBDFs = append(ibBDFs, e.Name())
		}
	}
	// os.ReadDir returns entries already sorted by name; zero-padded PCI BDFs
	// (DDDD:BB:SS.F) sort identically to BDF order, so no explicit sort needed.
	for i, bdf := range ibBDFs {
		if bdf == deviceID {
			return i, nil
		}
	}
	return -1, fmt.Errorf("device %s is not an InfiniBand controller on this node", deviceID)
}
