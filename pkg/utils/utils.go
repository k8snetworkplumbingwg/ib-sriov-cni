package utils

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	sriovConfigured = "/sriov_numvfs"
	// NetDirectory sysfs net directory
	NetDirectory = "/sys/class/net"
	// SysBusPci is sysfs pci device directory
	SysBusPci = "/sys/bus/pci/devices"
)

const (
	IPoIBAddrLengthBytes       = 20
	OwnerReadWriteExecuteAttrs = 0700
	OwnerReadWriteAttrs        = 0600
	VfioPciDriverName          = "vfio-pci"
	DefaultGUID                = "FF:FF:FF:FF:FF:FF:FF:FF"
)

// GetSriovNumVfs takes in a PF name(ifName) as string and returns number of VF configured as int
func GetSriovNumVfs(ifName string) (int, error) {
	var vfTotal int

	sriovFile := filepath.Join(NetDirectory, ifName, "device", sriovConfigured)
	if _, err := os.Lstat(sriovFile); err != nil {
		return vfTotal, fmt.Errorf("failed to open the sriov_numfs of device %q: %v", ifName, err)
	}

	data, err := os.ReadFile(sriovFile)
	if err != nil {
		return vfTotal, fmt.Errorf("failed to read the sriov_numfs of device %q: %v", ifName, err)
	}

	if len(data) == 0 {
		return vfTotal, fmt.Errorf("no data in the file %q", sriovFile)
	}

	sriovNumfs := strings.TrimSpace(string(data))
	vfTotal, err = strconv.Atoi(sriovNumfs)
	if err != nil {
		return vfTotal, fmt.Errorf("failed to convert sriov_numfs(byte value) to int of device %q: %v", ifName, err)
	}

	return vfTotal, nil
}

// GetVfid takes in VF's PCI address(addr) and pfName as string and returns VF's ID as int
func GetVfid(addr, pfName string) (int, error) {
	var id int
	vfTotal, err := GetSriovNumVfs(pfName)
	if err != nil {
		return id, err
	}
	for vf := 0; vf <= vfTotal; vf++ {
		vfDir := filepath.Join(NetDirectory, pfName, "device", fmt.Sprintf("virtfn%d", vf))
		_, err := os.Lstat(vfDir)
		if err != nil {
			continue
		}
		pciinfo, err := os.Readlink(vfDir)
		if err != nil {
			continue
		}
		pciaddr := filepath.Base(pciinfo)
		if pciaddr == addr {
			return vf, nil
		}
	}
	return id, fmt.Errorf("unable to get VF ID with PF: %s and VF pci address %v", pfName, addr)
}

// GetPfName returns PF net device name of a given VF pci address
func GetPfName(vf string) (string, error) {
	pfSymLink := filepath.Join(SysBusPci, vf, "physfn", "net")
	_, err := os.Lstat(pfSymLink)
	if err != nil {
		return "", err
	}

	files, err := os.ReadDir(pfSymLink)
	if err != nil {
		return "", err
	}

	if len(files) < 1 {
		return "", fmt.Errorf("PF network device not found")
	}

	return strings.TrimSpace(files[0].Name()), nil
}

// GetPciAddress takes in a interface(ifName) and VF id and returns returns its pci addr as string
func GetPciAddress(ifName string, vf int) (string, error) {
	var pciaddr string
	vfDir := filepath.Join(NetDirectory, ifName, "device", fmt.Sprintf("virtfn%d", vf))
	dirInfo, err := os.Lstat(vfDir)
	if err != nil {
		return pciaddr, fmt.Errorf("can't get the symbolic link of virtfn%d dir of the device %q: %v", vf, ifName, err)
	}

	if (dirInfo.Mode() & os.ModeSymlink) == 0 {
		return pciaddr, fmt.Errorf("no symbolic link for the virtfn%d dir of the device %q", vf, ifName)
	}

	pciinfo, err := os.Readlink(vfDir)
	if err != nil {
		return pciaddr, fmt.Errorf("can't read the symbolic link of virtfn%d dir of the device %q: %v", vf, ifName, err)
	}

	pciaddr = filepath.Base(pciinfo)
	return pciaddr, nil
}

// GetVFLinkNames returns VF's network interface name given it's PCI addr
func GetVFLinkNames(pciAddr string) (string, error) {
	vfDir := filepath.Join(SysBusPci, pciAddr, "net")
	if _, err := os.Lstat(vfDir); err != nil {
		return "", err
	}

	fInfos, err := os.ReadDir(vfDir)
	if err != nil {
		return "", fmt.Errorf("failed to read net dir of the device %s: %v", pciAddr, err)
	}

	if len(fInfos) == 0 {
		return "", fmt.Errorf("VF device %s sysfs path (%s) has no entries", pciAddr, vfDir)
	}

	names := make([]string, 0, len(fInfos))
	for _, f := range fInfos {
		names = append(names, f.Name())
	}

	return names[0], nil
}

// GetVFLinkNamesFromVFID returns VF's network interface name given it's PF name as string and VF id as int
func GetVFLinkNamesFromVFID(pfName string, vfID int) ([]string, error) {
	vfDir := filepath.Join(NetDirectory, pfName, "device", fmt.Sprintf("virtfn%d", vfID), "net")
	if _, err := os.Lstat(vfDir); err != nil {
		return nil, err
	}

	fInfos, err := os.ReadDir(vfDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read the virtfn%d dir of the device %q: %v", vfID, pfName, err)
	}

	names := make([]string, 0, len(fInfos))
	for _, f := range fInfos {
		names = append(names, f.Name())
	}

	return names, nil
}

// SaveNetConf takes in container ID, data dir and Pod interface name as string and a json encoded struct Conf
// and save this Conf in data dir
func SaveNetConf(cid, dataDir, podIfName string, conf interface{}) error {
	netConfBytes, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("error serializing delegate netconf: %v", err)
	}

	s := []string{cid, podIfName}
	cRef := strings.Join(s, "-")

	// save the rendered netconf for cmdDel
	err = saveScratchNetConf(cRef, dataDir, netConfBytes)
	if err != nil {
		return err
	}

	return nil
}

func saveScratchNetConf(containerID, dataDir string, netconf []byte) error {
	if err := os.MkdirAll(dataDir, OwnerReadWriteExecuteAttrs); err != nil {
		return fmt.Errorf("failed to create the sriov data directory(%q): %v", dataDir, err)
	}

	path := filepath.Join(dataDir, containerID)

	err := os.WriteFile(path, netconf, OwnerReadWriteAttrs)
	if err != nil {
		return fmt.Errorf("failed to write container data in the path(%q): %v", path, err)
	}

	return err
}

// ReadScratchNetConf takes in container ID, Pod interface name and data dir as string and returns a pointer to Conf
func ReadScratchNetConf(cRefPath string) ([]byte, error) {
	data, err := os.ReadFile(cRefPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read container data in the path(%q): %v", cRefPath, err)
	}

	return data, err
}

// CleanCachedNetConf removed cached NetConf from disk
func CleanCachedNetConf(cRefPath string) error {
	if err := os.Remove(cRefPath); err != nil {
		return fmt.Errorf("error removing NetConf file %s: %q", cRefPath, err)
	}
	return nil
}

// Return GUID string, extracted from hardware address if it is IPoIB, or empty string
func GetGUIDFromHwAddr(hwAddr net.HardwareAddr) string {
	if len(hwAddr) == IPoIBAddrLengthBytes {
		// GUID is lower 8 bytes of hardware address
		return hwAddr[12:].String()
	}
	return ""
}

// IsValidGUID check if the guid is valid
func IsValidGUID(guid string) bool {
	if IsAllZeroGUID(guid) {
		return false
	}

	match, _ := regexp.MatchString("^([0-9a-fA-F]{2}:){7}[0-9a-fA-F]{2}$", guid)
	return match
}

// IsAllZeroGUID check if the guid is all zero which is invalid guid
func IsAllZeroGUID(guid string) bool {
	return guid == "00:00:00:00:00:00:00:00"
}

// IsAllOnesGUID check if the guid is all ones
func IsAllOnesGUID(guid string) bool {
	return guid == "ff:ff:ff:ff:ff:ff:ff:ff" || guid == "FF:FF:FF:FF:FF:FF:FF:FF"
}

// IsVfioPciDevice checks if a PCI device is bound to vfio-pci driver
func IsVfioPciDevice(pciAddr string) (bool, error) {
	driverPath := filepath.Join(SysBusPci, pciAddr, "driver")

	// Check if driver symlink exists
	linkTarget, err := os.Readlink(driverPath)
	if err != nil {
		// If readlink fails, the device might not be bound to any driver
		return false, nil
	}

	// Check if the driver is vfio-pci
	driverName := filepath.Base(linkTarget)
	return driverName == VfioPciDriverName, nil
}
