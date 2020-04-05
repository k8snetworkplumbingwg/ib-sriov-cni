package sriov

import (
	"errors"
	"net"

	"github.com/Mellanox/ib-sriov-cni/pkg/types"
	"github.com/Mellanox/ib-sriov-cni/pkg/types/mocks"

	"github.com/containernetworking/plugins/pkg/ns"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"
)

// FakeLink is a dummy netlink struct used during testing
type FakeLink struct {
	netlink.LinkAttrs
}

func (l *FakeLink) Attrs() *netlink.LinkAttrs {
	return &l.LinkAttrs
}

func (l *FakeLink) Type() string {
	return "FakeLink"
}

// Fake NS - implements ns.NetNS interface
type fakeNetNS struct {
	closed bool
	fd     uintptr
	path   string
}

func (f *fakeNetNS) Do(toRun func(ns.NetNS) error) error {
	return toRun(f)
}

func (f *fakeNetNS) Set() error {
	return nil
}

func (f *fakeNetNS) Path() string {
	return f.path
}

func (f *fakeNetNS) Fd() uintptr {
	return f.fd
}

func (f *fakeNetNS) Close() error {
	f.closed = true
	return nil
}

func newFakeNs() ns.NetNS {
	return &fakeNetNS{
		closed: false,
		fd:     17,
		path:   "/proc/4123/ns/net",
	}
}

var _ = Describe("Sriov", func() {

	Context("Checking ApplyVFConfig function", func() {
		var (
			netconf *types.NetConf
		)

		BeforeEach(func() {
			netconf = &types.NetConf{
				Master:      "ibFake0",
				DeviceID:    "0000:af:06.0",
				VFID:        0,
				HostIFNames: "ibFake5",
			}
		})

		It("ApplyVFConfig with valid GUID", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}
			mockedPciUtils := &mocks.PciUtils{}
			hostGuid := "11:22:33:00:00:aa:bb:cc"
			gid, err := net.ParseMAC("00:00:04:a5:fe:80:00:00:00:00:00:00:" + hostGuid)
			Expect(err).ToNot(HaveOccurred())

			fakeLink := &FakeLink{netlink.LinkAttrs{
				HardwareAddr: gid,
			}}
			netconf.GUID = "01:23:45:67:89:ab:cd:ef"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkSetVfNodeGUID", fakeLink, mock.AnythingOfType("int"), mock.Anything).Return(nil)
			mockedNetLinkManger.On("LinkSetVfPortGUID", fakeLink, mock.AnythingOfType("int"), mock.Anything).Return(nil)

			mockedPciUtils.On("RebindVf", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(nil)

			sm := sriovManager{nLink: mockedNetLinkManger, utils: mockedPciUtils}
			err = sm.ApplyVFConfig(netconf)
			Expect(err).NotTo(HaveOccurred())
			Expect(netconf.HostIFGUID).To(Equal(hostGuid))
		})
		It("ApplyVFConfig with invalid GUID - wrong characters", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.GUID = "invalid GUID"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)

			sm := sriovManager{nLink: mockedNetLinkManger}
			err := sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
		})
		It("ApplyVFConfig with invalid GUID - wrong length", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.GUID = "00:11:22:33:44:55:66"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)

			sm := sriovManager{nLink: mockedNetLinkManger}
			err := sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
		})
		It("ApplyVFConfig with invalid GUID - all zeros guid", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.GUID = "00:00:00:00:00:00:00:00"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)

			sm := sriovManager{nLink: mockedNetLinkManger}
			err := sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
		})
		It("ApplyVFConfig with invalid GUID - invalid guid address", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.GUID = "00:AF-3B-0123:21:3322"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)

			sm := sriovManager{nLink: mockedNetLinkManger}
			err := sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
		})
		It("ApplyVFConfig check guid - failed to get vf link", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}
			mockedPciUtils := &mocks.PciUtils{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.GUID = "01:23:45:67:89:ab:cd:ef"

			mockedNetLinkManger.On("LinkByName", netconf.Master).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkByName", netconf.HostIFNames).Return(nil, errors.New("mocked failed"))

			sm := sriovManager{nLink: mockedNetLinkManger, utils: mockedPciUtils}
			err := sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(`failed to lookup vf "ibFake5": mocked failed`))
		})
		It("ApplyVFConfig check guid - failed to set node guid", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}

			hostGuid := "11:22:33:00:00:aa:bb:cc"
			gid, err := net.ParseMAC("00:00:04:a5:fe:80:00:00:00:00:00:00:" + hostGuid)
			Expect(err).ToNot(HaveOccurred())

			fakeLink := &FakeLink{netlink.LinkAttrs{
				HardwareAddr: gid,
			}}

			netconf.GUID = "01:23:45:67:89:ab:cd:ef"

			mockedNetLinkManger.On("LinkByName", mock.Anything).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkSetVfNodeGUID", fakeLink, mock.Anything, mock.Anything).Return(errors.New("mocked failed"))

			sm := sriovManager{nLink: mockedNetLinkManger}
			err = sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(`failed to add node guid 01:23:45:67:89:ab:cd:ef: mocked failed`))
			Expect(netconf.HostIFGUID).To(Equal(hostGuid))
		})
		It("ApplyVFConfig check guid - failed to set port guid", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}

			hostGuid := "11:22:33:00:00:aa:bb:cc"
			gid, err := net.ParseMAC("00:00:04:a5:fe:80:00:00:00:00:00:00:" + hostGuid)
			Expect(err).ToNot(HaveOccurred())

			fakeLink := &FakeLink{netlink.LinkAttrs{
				HardwareAddr: gid,
			}}

			netconf.GUID = "01:23:45:67:89:ab:cd:ef"

			mockedNetLinkManger.On("LinkByName", mock.Anything).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkSetVfNodeGUID", fakeLink, mock.Anything, mock.Anything).Return(nil)
			mockedNetLinkManger.On("LinkSetVfPortGUID", fakeLink, mock.Anything, mock.Anything).Return(errors.New("mocked failed"))

			sm := sriovManager{nLink: mockedNetLinkManger}
			err = sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(`failed to add port guid 01:23:45:67:89:ab:cd:ef: mocked failed`))
			Expect(netconf.HostIFGUID).To(Equal(hostGuid))
		})
		It("ApplyVFConfig check guid - failed to rebind after set guid", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}
			mockedPciUtils := &mocks.PciUtils{}

			hostGuid := "11:22:33:00:00:aa:bb:cc"
			gid, err := net.ParseMAC("00:00:04:a5:fe:80:00:00:00:00:00:00:" + hostGuid)
			Expect(err).ToNot(HaveOccurred())

			fakeLink := &FakeLink{netlink.LinkAttrs{
				HardwareAddr: gid,
			}}

			netconf.GUID = "01:23:45:67:89:ab:cd:ef"

			mockedNetLinkManger.On("LinkByName", mock.Anything).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkSetVfNodeGUID", fakeLink, mock.Anything, mock.Anything).Return(nil)
			mockedNetLinkManger.On("LinkSetVfPortGUID", fakeLink, mock.Anything, mock.Anything).Return(nil)

			mockedPciUtils.On("RebindVf", netconf.Master, netconf.DeviceID).Return(errors.New("mocked failed"))

			sm := sriovManager{nLink: mockedNetLinkManger, utils: mockedPciUtils}
			err = sm.ApplyVFConfig(netconf)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("mocked failed"))
			Expect(netconf.HostIFGUID).To(Equal(hostGuid))
		})
	})
	Context("Checking SetupVF function", func() {
		var (
			podifName string
			contID    string
			netconf   *types.NetConf
		)

		BeforeEach(func() {
			podifName = "net1"
			contID = "dummycid"
			netconf = &types.NetConf{
				Master:      "ib0",
				DeviceID:    "0000:af:06.0",
				VFID:        0,
				HostIFNames: "ib1",
				ContIFNames: "net1",
			}
		})

		It("Assuming existing interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{
				Index: 1000,
				Name:  "dummylink",
			}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(nil)
			mocked.On("LinkSetNsFd", fakeLink, mock.AnythingOfType("int")).Return(nil)
			mocked.On("LinkSetUp", fakeLink).Return(nil)
			sm := sriovManager{nLink: mocked}
			err := sm.SetupVF(netconf, podifName, contID, targetNetNS)
			Expect(err).NotTo(HaveOccurred())
		})
		It("Assuming non existing interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(nil, errors.New("not fount"))
			sm := sriovManager{nLink: mocked}
			err := sm.SetupVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming existing interface not able to set down", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{
				Index: 1000,
				Name:  "dummylink",
			}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.SetupVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming failed to change name", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{
				Index: 1000,
				Name:  "dummylink",
			}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.SetupVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming failed to move interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{
				Index: 1000,
				Name:  "dummylink",
			}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(nil)
			mocked.On("LinkSetNsFd", fakeLink, mock.Anything).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.SetupVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
	})
	Context("Checking ReleaseVF function", func() {
		var (
			podifName string
			contID    string
			netconf   *types.NetConf
		)

		BeforeEach(func() {
			podifName = "net1"
			contID = "dummycid"
			netconf = &types.NetConf{
				Master:      "ib0",
				DeviceID:    "0000:af:06.0",
				VFID:        0,
				HostIFNames: "ib1",
				ContIFNames: "net1",
			}
		})
		It("Assuming existing interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}
			fakeLink := &FakeLink{netlink.LinkAttrs{Index: 1000, Name: "dummylink"}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(nil)
			mocked.On("LinkSetNsFd", fakeLink, mock.AnythingOfType("int")).Return(nil)
			sm := sriovManager{nLink: mocked}
			err := sm.ReleaseVF(netconf, podifName, contID, targetNetNS)
			Expect(err).NotTo(HaveOccurred())
		})
		It("Assuming non existing interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(nil, errors.New("not found"))
			sm := sriovManager{nLink: mocked}
			err := sm.ReleaseVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming failed to set interface down", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}
			fakeLink := &FakeLink{netlink.LinkAttrs{Index: 1000, Name: "dummylink"}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.ReleaseVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming failed to move interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}
			fakeLink := &FakeLink{netlink.LinkAttrs{Index: 1000, Name: "dummylink"}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.ReleaseVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming existing interface", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}
			fakeLink := &FakeLink{netlink.LinkAttrs{Index: 1000, Name: "dummylink"}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(nil)
			mocked.On("LinkSetNsFd", fakeLink, mock.AnythingOfType("int")).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.ReleaseVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
		It("Assuming failed to set interface up after moving", func() {
			targetNetNS := newFakeNs()
			mocked := &mocks.NetlinkManager{}

			fakeLink := &FakeLink{netlink.LinkAttrs{
				Index: 1000,
				Name:  "dummylink",
			}}

			mocked.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mocked.On("LinkSetDown", fakeLink).Return(nil)
			mocked.On("LinkSetName", fakeLink, mock.Anything).Return(nil)
			mocked.On("LinkSetNsFd", fakeLink, mock.AnythingOfType("int")).Return(nil)
			mocked.On("LinkSetUp", fakeLink).Return(errors.New("failed"))
			sm := sriovManager{nLink: mocked}
			err := sm.SetupVF(netconf, podifName, contID, targetNetNS)
			Expect(err).To(HaveOccurred())
		})
	})
	Context("Checking ResetVFConfig function", func() {
		var (
			netconf *types.NetConf
		)

		BeforeEach(func() {
			netconf = &types.NetConf{
				Master:      "i4",
				DeviceID:    "0000:af:06.0",
				VFID:        0,
				HostIFNames: "i1",
			}
		})

		It("ResetVFConfig with valid GUID", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}
			mockedPciUtils := &mocks.PciUtils{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.HostIFGUID = "01:23:45:67:89:ab:cd:ef"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkSetVfNodeGUID", fakeLink, mock.AnythingOfType("int"), mock.Anything).Return(nil)
			mockedNetLinkManger.On("LinkSetVfPortGUID", fakeLink, mock.AnythingOfType("int"), mock.Anything).Return(nil)

			mockedPciUtils.On("RebindVf", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(nil)

			sm := sriovManager{nLink: mockedNetLinkManger, utils: mockedPciUtils}
			err := sm.ResetVFConfig(netconf)
			Expect(err).NotTo(HaveOccurred())
		})
		It("ResetVFConfig with GUID all zeros", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}
			mockedPciUtils := &mocks.PciUtils{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.HostIFGUID = "00:00:00:00:00:00:00:00"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)
			mockedNetLinkManger.On("LinkSetVfNodeGUID", fakeLink, mock.AnythingOfType("int"), mock.Anything).Return(nil)
			mockedNetLinkManger.On("LinkSetVfPortGUID", fakeLink, mock.AnythingOfType("int"), mock.Anything).Return(nil)

			mockedPciUtils.On("RebindVf", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(nil)

			sm := sriovManager{nLink: mockedNetLinkManger, utils: mockedPciUtils}
			err := sm.ResetVFConfig(netconf)
			Expect(err).NotTo(HaveOccurred())
			Expect(netconf.HostIFGUID).To(Equal("FF:FF:FF:FF:FF:FF:FF:FF"))
		})
		It("ResetVFConfig with invalid GUID", func() {
			mockedNetLinkManger := &mocks.NetlinkManager{}
			mockedPciUtils := &mocks.PciUtils{}

			fakeLink := &FakeLink{netlink.LinkAttrs{}}
			netconf.HostIFGUID = "12312-123:434"

			mockedNetLinkManger.On("LinkByName", mock.AnythingOfType("string")).Return(fakeLink, nil)

			sm := sriovManager{nLink: mockedNetLinkManger, utils: mockedPciUtils}
			err := sm.ResetVFConfig(netconf)
			Expect(err).To(HaveOccurred())
		})
	})
})
