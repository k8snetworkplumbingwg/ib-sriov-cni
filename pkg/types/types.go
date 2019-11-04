package types

import (
	"github.com/containernetworking/cni/pkg/types"
)

// NetConf extends types.NetConf for ib-sriov-cni
type NetConf struct {
	types.NetConf
	Master      string
	DeviceID    string `json:"deviceID"` // PCI address of a VF in valid sysfs format
	VFID        int
	HostIFNames string // VF netdevice name(s)
	ContIFNames string // VF names after in the container; used during deletion
	GUID        string `json:"guid"`
	LinkState   string `json:"link_state,omitempty"` // auto|enable|disable
}
