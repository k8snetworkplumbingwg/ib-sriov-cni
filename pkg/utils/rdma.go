package utils

import (
	"fmt"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/k8snetworkplumbingwg/rdma-cni/pkg/rdma"
)

var rdmaManager = rdma.NewRdmaManager()

// Ensure RDMA subsystem mode is set to exclusive.
func EnsureRdmaSystemMode() error {
	mode, err := rdmaManager.GetSystemRdmaMode()
	if err != nil {
		return fmt.Errorf("failed to get RDMA subsystem namespace awareness mode. %v", err)
	}
	if mode != rdma.RdmaSysModeExclusive {
		return fmt.Errorf("RDMA subsystem namespace awareness mode is set to %s, "+
			"expecting it to be set to %s, invalid system configurations", mode, rdma.RdmaSysModeExclusive)
	}
	return nil
}

// Move RDMA device to namespace
func MoveRdmaDevToNs(rdmaDev string, targetNs ns.NetNS) error {
	err := rdmaManager.MoveRdmaDevToNs(rdmaDev, targetNs)
	if err != nil {
		return fmt.Errorf("failed to move RDMA device %s to namespace. %v", rdmaDev, err)
	}
	return nil
}

// Move RDMA device to namespace
func MoveRdmaDevToNsPci(pciDev string, targetNs ns.NetNS) (string, error) { // (hostRdmaDev, error)
	rdmaDevs := rdmaManager.GetRdmaDevsForPciDev(pciDev)
	if len(rdmaDevs) == 0 {
		return "", fmt.Errorf("failed to get RDMA devices for PCI device: %s. No RDMA devices found", pciDev)
	}

	if len(rdmaDevs) != 1 {
		// Expecting exactly one RDMA device
		return "", fmt.Errorf(
			"discovered more than one RDMA device %v for PCI device %s. Unsupported state", rdmaDevs, pciDev)
	}

	// Move RDMA device to container namespace
	rdmaDev := rdmaDevs[0]

	err := MoveRdmaDevToNs(rdmaDev, targetNs)
	if err != nil {
		return "", fmt.Errorf("failed to move RDMA device %s to namespace. %v", rdmaDev, err)
	}
	return rdmaDev, nil
}

// Move RDMA device from namespace to current (default) namespace
func MoveRdmaDevFromNs(rdmaDev string, sourceNs ns.NetNS) error {
	targetNs, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to open current network namespace: %v", err)
	}
	defer targetNs.Close()

	err = sourceNs.Do(func(_ ns.NetNS) error {
		// Move RDMA device to default namespace
		return rdmaManager.MoveRdmaDevToNs(rdmaDev, targetNs)
	})
	if err != nil {
		return fmt.Errorf("failed to move RDMA device %s to default namespace. %v", rdmaDev, err)
	}
	return err
}
