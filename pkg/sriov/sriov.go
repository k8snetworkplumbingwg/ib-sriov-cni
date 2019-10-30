package sriov

import (
	"fmt"
	"net"

	"github.com/containernetworking/plugins/pkg/ns"

	"github.com/Mellanox/ib-sriov-cni/pkg/types"
	"github.com/Mellanox/ib-sriov-cni/pkg/utils"
	"github.com/vishvananda/netlink"
)

// mocked netlink interface
// required for unit tests

// NetlinkManager is an interface to mock nelink library
type NetlinkManager interface {
	LinkByName(string) (netlink.Link, error)
	LinkSetUp(netlink.Link) error
	LinkSetDown(netlink.Link) error
	LinkSetNsFd(netlink.Link, int) error
	LinkSetName(netlink.Link, string) error
	LinkSetVfTxRate(netlink.Link, int, int) error
	LinkSetVfSpoofchk(netlink.Link, int, bool) error
	LinkSetVfTrust(netlink.Link, int, bool) error
	LinkSetVfState(netlink.Link, int, uint32) error
}

// MyNetlink NetlinkManager
type MyNetlink struct {
	lm NetlinkManager
}

// LinkByName implements NetlinkManager
func (n *MyNetlink) LinkByName(name string) (netlink.Link, error) {
	return netlink.LinkByName(name)
}

// LinkSetVfVlan using NetlinkManager
func (n *MyNetlink) LinkSetVfVlan(link netlink.Link, vf, vlan int) error {
	return netlink.LinkSetVfVlan(link, vf, vlan)
}

// LinkSetVfVlanQos sets VLAN ID and QoS field for given VF using NetlinkManager
func (n *MyNetlink) LinkSetVfVlanQos(link netlink.Link, vf, vlan, qos int) error {
	return netlink.LinkSetVfVlanQos(link, vf, vlan, qos)
}

// LinkSetVfHardwareAddr using NetlinkManager
func (n *MyNetlink) LinkSetVfHardwareAddr(link netlink.Link, vf int, hwaddr net.HardwareAddr) error {
	return netlink.LinkSetVfHardwareAddr(link, vf, hwaddr)
}

// LinkSetHardwareAddr using NetlinkManager
func (n *MyNetlink) LinkSetHardwareAddr(link netlink.Link, hwaddr net.HardwareAddr) error {
	return netlink.LinkSetHardwareAddr(link, hwaddr)
}

// LinkSetUp using NetlinkManager
func (n *MyNetlink) LinkSetUp(link netlink.Link) error {
	return netlink.LinkSetUp(link)
}

// LinkSetDown using NetlinkManager
func (n *MyNetlink) LinkSetDown(link netlink.Link) error {
	return netlink.LinkSetDown(link)
}

// LinkSetNsFd using NetlinkManager
func (n *MyNetlink) LinkSetNsFd(link netlink.Link, fd int) error {
	return netlink.LinkSetNsFd(link, fd)
}

// LinkSetName using NetlinkManager
func (n *MyNetlink) LinkSetName(link netlink.Link, name string) error {
	return netlink.LinkSetName(link, name)
}

// LinkSetVfTxRate using NetlinkManager
func (n *MyNetlink) LinkSetVfTxRate(link netlink.Link, vf int, rate int) error {
	return netlink.LinkSetVfTxRate(link, vf, rate)
}

// LinkSetVfSpoofchk using NetlinkManager
func (n *MyNetlink) LinkSetVfSpoofchk(link netlink.Link, vf int, check bool) error {
	return netlink.LinkSetVfSpoofchk(link, vf, check)
}

// LinkSetVfTrust using NetlinkManager
func (n *MyNetlink) LinkSetVfTrust(link netlink.Link, vf int, state bool) error {
	return netlink.LinkSetVfTrust(link, vf, state)
}

// LinkSetVfState using NetlinkManager
func (n *MyNetlink) LinkSetVfState(link netlink.Link, vf int, state uint32) error {
	return netlink.LinkSetVfState(link, vf, state)
}

type pciUtils interface {
	getSriovNumVfs(ifName string) (int, error)
	getVFLinkNamesFromVFID(pfName string, vfID int) ([]string, error)
	getPciAddress(ifName string, vf int) (string, error)
}

type pciUtilsImpl struct{}

func (p *pciUtilsImpl) getSriovNumVfs(ifName string) (int, error) {
	return utils.GetSriovNumVfs(ifName)
}

func (p *pciUtilsImpl) getVFLinkNamesFromVFID(pfName string, vfID int) ([]string, error) {
	return utils.GetVFLinkNamesFromVFID(pfName, vfID)
}

func (p *pciUtilsImpl) getPciAddress(ifName string, vf int) (string, error) {
	return utils.GetPciAddress(ifName, vf)
}

// Manager provides interface invoke sriov nic related operations
type Manager interface {
	SetupVF(conf *types.NetConf, podifName string, cid string, netns ns.NetNS) error
	ReleaseVF(conf *types.NetConf, podifName string, cid string, netns ns.NetNS) error
	ResetVFConfig(conf *types.NetConf) error
	ApplyVFConfig(conf *types.NetConf) error
}

type sriovManager struct {
	nLink NetlinkManager
	utils pciUtils
}

// NewSriovManager returns an instance of SriovManager
func NewSriovManager() Manager {
	return &sriovManager{
		nLink: &MyNetlink{},
		utils: &pciUtilsImpl{},
	}
}

// SetupVF sets up a VF in Pod netns
func (s *sriovManager) SetupVF(conf *types.NetConf, podifName string, cid string, netns ns.NetNS) error {
	linkName := conf.HostIFNames

	linkObj, err := s.nLink.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("error getting VF netdevice with name %s", linkName)
	}

	// tempName used as intermediary name to avoid name conflicts
	tempName := fmt.Sprintf("%s%d", linkName, linkObj.Attrs().Index)

	// 1. Set link down
	if err := s.nLink.LinkSetDown(linkObj); err != nil {
		return fmt.Errorf("failed to down vf device %q: %v", linkName, err)
	}

	// 2. Set temp name
	if err := s.nLink.LinkSetName(linkObj, tempName); err != nil {
		return fmt.Errorf("error setting temp IF name %s for %s", tempName, linkName)
	}

	// 3. Change netns
	if err := s.nLink.LinkSetNsFd(linkObj, int(netns.Fd())); err != nil {
		return fmt.Errorf("failed to move IF %s to netns: %q", tempName, err)
	}

	if err := netns.Do(func(_ ns.NetNS) error {
		// 4. Set Pod IF name
		if err := s.nLink.LinkSetName(linkObj, podifName); err != nil {
			return fmt.Errorf("error setting container interface name %s for %s", linkName, tempName)
		}

		// 5. Bring IF up in Pod netns
		if err := s.nLink.LinkSetUp(linkObj); err != nil {
			return fmt.Errorf("error bringing interface up in container ns: %q", err)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("error setting up interface in container namespace: %q", err)
	}
	conf.ContIFNames = podifName

	return nil
}

// ReleaseVF reset a VF from Pod netns and return it to init netns
func (s *sriovManager) ReleaseVF(conf *types.NetConf, podifName string, cid string, netns ns.NetNS) error {

	initns, err := ns.GetCurrentNS()
	if err != nil {
		return fmt.Errorf("failed to get init netns: %v", err)
	}

	if len(conf.ContIFNames) < 1 && len(conf.ContIFNames) != len(conf.HostIFNames) {
		return fmt.Errorf("number of interface names mismatch ContIFNames: %d HostIFNames: %d", len(conf.ContIFNames), len(conf.HostIFNames))
	}

	return netns.Do(func(_ ns.NetNS) error {

		// get VF device
		linkObj, err := s.nLink.LinkByName(podifName)
		if err != nil {
			return fmt.Errorf("failed to get netlink device with name %s: %q", podifName, err)
		}

		// shutdown VF device
		if err = s.nLink.LinkSetDown(linkObj); err != nil {
			return fmt.Errorf("failed to set link %s down: %q", podifName, err)
		}

		// rename VF device
		err = s.nLink.LinkSetName(linkObj, conf.HostIFNames)
		if err != nil {
			return fmt.Errorf("failed to rename link %s to host name %s: %q", podifName, conf.HostIFNames, err)
		}

		// move VF device to init netns
		if err = s.nLink.LinkSetNsFd(linkObj, int(initns.Fd())); err != nil {
			return fmt.Errorf("failed to move interface %s to init netns: %v", conf.HostIFNames, err)
		}

		return nil
	})
}

// ApplyVFConfig configure a VF with parameters given in NetConf
func (s *sriovManager) ApplyVFConfig(conf *types.NetConf) error {

	pfLink, err := s.nLink.LinkByName(conf.Master)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", conf.Master, err)
	}

	// Set link state
	if conf.LinkState != "" {
		var state uint32
		switch conf.LinkState {
		case "auto":
			state = netlink.VF_LINK_STATE_AUTO
		case "enable":
			state = netlink.VF_LINK_STATE_ENABLE
		case "disable":
			state = netlink.VF_LINK_STATE_DISABLE
		default:
			// the value should have been validated earlier, return error if we somehow got here
			return fmt.Errorf("unknown link state %s when setting it for vf %d: %v", conf.LinkState, conf.VFID, err)
		}
		if err = s.nLink.LinkSetVfState(pfLink, conf.VFID, state); err != nil {
			return fmt.Errorf("failed to set vf %d link state to %d: %v", conf.VFID, state, err)
		}
	}

	return nil
}

// ResetVFConfig reset a VF with default values
func (s *sriovManager) ResetVFConfig(conf *types.NetConf) error {

	pfLink, err := s.nLink.LinkByName(conf.Master)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", conf.Master, err)
	}

	// Set spoofchk to on
	if err = s.nLink.LinkSetVfSpoofchk(pfLink, conf.VFID, true); err != nil {
		return fmt.Errorf("failed to enable spoof checking for vf %d: %v", conf.VFID, err)
	}

	// Set trust to off
	if err = s.nLink.LinkSetVfTrust(pfLink, conf.VFID, false); err != nil {
		return fmt.Errorf("failed to disable trust for vf %d: %v", conf.VFID, err)
	}

	// Disable rate limiting
	if err = s.nLink.LinkSetVfTxRate(pfLink, conf.VFID, 0); err != nil {
		return fmt.Errorf("failed to disable rate limiting for vf %d %v", conf.VFID, err)
	}

	// Reset link state to `auto`
	if conf.LinkState != "" {
		// While resetting to `auto` can be a reasonable thing to do regardless of whether it was explicitly
		// specified in the network definition, reset only when link_state was explicitly specified, to
		// accommodate for drivers / NICs that don't support the netlink command (e.g. igb driver)
		if err = s.nLink.LinkSetVfState(pfLink, conf.VFID, 0); err != nil {
			return fmt.Errorf("failed to set link state to auto for vf %d: %v", conf.VFID, err)
		}
	}

	return nil
}
