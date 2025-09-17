package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

var (
	// DefaultCNIDir used for caching NetConf
	DefaultCNIDir = "/var/lib/cni/ib-sriov"
	// CniFileLockDir point to the CNI's lockfile
	CniFileLockDir = "/var/run/cni/ib-sriov"
	// CniFileLockName is the name of the lockfile used in the CNI
	CniFileLockName = "cni.lock"
)

// LoadConf parses and validates stdin netconf and returns NetConf object
func LoadConf(bytes []byte) (*types.NetConf, error) {
	n := &types.NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	// validate that link state is one of supported values
	if n.LinkState != "" && n.LinkState != "auto" && n.LinkState != "enable" && n.LinkState != "disable" {
		return nil, fmt.Errorf("invalid link_state value: %s", n.LinkState)
	}
	return n, nil
}

// Load device specific information into netConf
func LoadDeviceInfo(netConf *types.NetConf) error {
	// DeviceID takes precedence; if we are given a VF pciaddr then work from there
	if netConf.DeviceID != "" {
		// Get rest of the VF information
		pfName, vfID, err := getVfInfo(netConf.DeviceID)
		if err != nil {
			return fmt.Errorf("load config: failed to get VF information: %q", err)
		}
		netConf.VFID = vfID
		netConf.Master = pfName
	} else {
		return fmt.Errorf("load config: vf pci addr is required")
	}

	// Get interface name
	hostIFNames, err := utils.GetVFLinkNames(netConf.DeviceID)
	if err != nil || hostIFNames == "" {
		return fmt.Errorf("load config: failed to detect VF %s name with error, %q", netConf.DeviceID, err)
	}

	netConf.HostIFNames = hostIFNames
	return nil
}

// LoadDeviceInfoVfioVF loads device information for VFIO VF devices (no network interface)
func LoadDeviceInfoVfioVF(netConf *types.NetConf) error {
	// DeviceID takes precedence; if we are given a VF pciaddr then work from there
	if netConf.DeviceID != "" {
		// Get rest of the VF information
		pfName, vfID, err := getVfInfo(netConf.DeviceID)
		if err != nil {
			return fmt.Errorf("load config: failed to get VF information: %q", err)
		}
		netConf.VFID = vfID
		netConf.Master = pfName

		// For VFIO VF, we don't have network interface, so set HostIFNames to empty
		netConf.HostIFNames = ""
	} else {
		return fmt.Errorf("load config: vf pci addr is required")
	}

	return nil
}

func getVfInfo(vfPci string) (string, int, error) {
	var vfID int

	pf, err := utils.GetPfName(vfPci)
	if err != nil {
		return "", vfID, err
	}

	vfID, err = utils.GetVfid(vfPci, pf)
	if err != nil {
		return "", vfID, err
	}

	return pf, vfID, nil
}

// LoadConfFromCache retrieves cached NetConf returns it along with a handle for removal
func LoadConfFromCache(args *skel.CmdArgs) (*types.NetConf, string, error) {
	netConf := &types.NetConf{}

	s := []string{args.ContainerID, args.IfName}
	cRef := strings.Join(s, "-")
	cRefPath := filepath.Join(DefaultCNIDir, cRef)

	netConfBytes, err := utils.ReadScratchNetConf(cRefPath)
	if err != nil {
		return nil, "", fmt.Errorf("error reading cached NetConf in %s with name %s", DefaultCNIDir, cRef)
	}

	if err = json.Unmarshal(netConfBytes, netConf); err != nil {
		return nil, "", fmt.Errorf("failed to parse NetConf: %q", err)
	}

	return netConf, cRefPath, nil
}
