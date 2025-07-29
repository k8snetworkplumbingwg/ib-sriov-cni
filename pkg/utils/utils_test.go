package utils

import (
	"net"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Utils", func() {

	Context("Checking GetSriovNumVfs function", func() {
		It("Assuming existing interface", func() {
			result, err := GetSriovNumVfs("ib0")
			Expect(result).To(Equal(2), "Existing sriov interface should return correct VFs count")
			Expect(err).NotTo(HaveOccurred(), "Existing sriov interface should not return an error")
		})
		It("Assuming not existing interface", func() {
			_, err := GetSriovNumVfs("enp175s0f2")
			Expect(err).To(HaveOccurred(), "Not existing sriov interface should return an error")
		})
	})
	Context("Checking GetVfid function", func() {
		It("Assuming existing interface", func() {
			result, err := GetVfid("0000:af:06.0", "ib0")
			Expect(result).To(Equal(0), "Existing VF should return correct VF index")
			Expect(err).NotTo(HaveOccurred(), "Existing VF should not return an error")
		})
		It("Assuming not existing interface", func() {
			_, err := GetVfid("0000:af:06.0", "enp175s0f2")
			Expect(err).To(HaveOccurred(), "Not existing interface should return an error")
		})
	})
	Context("Checking GetPfName function", func() {
		It("Assuming existing vf", func() {
			result, err := GetPfName("0000:af:06.0")
			Expect(err).NotTo(HaveOccurred(), "Existing VF should not return an error")
			Expect(result).To(Equal("ib0"), "Existing VF should return correct PF name")
		})
		It("Assuming not existing vf", func() {
			result, err := GetPfName("0000:af:07.0")
			Expect(result).To(Equal(""))
			Expect(err).To(HaveOccurred(), "Not existing VF should return an error")
		})
	})
	Context("Checking GetPciAddress function", func() {
		It("Assuming existing interface and vf", func() {
			Expect(GetPciAddress("ib0", 0)).To(Equal("0000:af:06.0"),
				"Existing PF and VF id should return correct VF pci address")
		})
		It("Assuming not existing interface", func() {
			_, err := GetPciAddress("enp175s0f2", 0)
			Expect(err).To(HaveOccurred(), "Not existing PF should return an error")
		})
		It("Assuming not existing vf", func() {
			result, err := GetPciAddress("ib0", 33)
			Expect(result).To(Equal(""), "Not existing VF id should not return pci address")
			Expect(err).To(HaveOccurred(), "Not existing VF id should return an error")
		})
	})
	Context("Checking GetVFLinkNames function", func() {
		It("Assuming existing vf", func() {
			result, err := GetVFLinkNamesFromVFID("ib0", 0)
			Expect(result).To(ContainElement("ib1"), "Existing PF should have at least one VF")
			Expect(err).NotTo(HaveOccurred(), "Existing PF should not return an error")
		})
		It("Assuming not existing vf", func() {
			_, err := GetVFLinkNamesFromVFID("ib0", 3)
			Expect(err).To(HaveOccurred(), "Not existing VF should return an error")
		})
	})
	Context("Checking GetGUIDFromHwAddr function", func() {
		It("Valid IPoIB hardware address", func() {
			hwAddr, _ := net.ParseMAC("00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01")
			guid := GetGUIDFromHwAddr(hwAddr)
			Expect(guid).To(Equal("02:00:5e:10:00:00:00:01"))
		})
		It("Not valid IPoIB hardware address", func() {
			hwAddr, _ := net.ParseMAC("00:00:00:00:fe:80:00:00")
			guid := GetGUIDFromHwAddr(hwAddr)
			Expect(guid).To(Equal(""))
		})
	})
	Context("Checking IsVfioPciDevice function", func() {
		It("Assuming device bound to vfio-pci driver", func() {
			// Test with VF (0000:af:06.1) that is bound to vfio-pci in the mock
			result, err := IsVfioPciDevice("0000:af:06.1")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for valid PCI address")
			Expect(result).To(Equal(true), "Device bound to vfio-pci driver should return true")
		})
		It("Assuming PF device bound to mlx5_core driver", func() {
			// Test with PF (0000:af:00.1) that should be bound to mlx5_core
			result, err := IsVfioPciDevice("0000:af:00.1")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for valid PCI address")
			Expect(result).To(Equal(false), "PF bound to non-vfio driver should return false")
		})
		It("Assuming VF device bound to mlx5_core driver", func() {
			// Test with VF (0000:af:06.0) that should be bound to mlx5_core
			result, err := IsVfioPciDevice("0000:af:06.0")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for valid PCI address")
			Expect(result).To(Equal(false), "VF bound to non-vfio driver should return false")
		})
		It("Assuming device not bound to any driver", func() {
			// Non-existing device path will fail readlink, returning false
			result, err := IsVfioPciDevice("0000:ff:ff.f")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for unbound device")
			Expect(result).To(Equal(false), "Device not bound to driver should return false")
		})
	})
})
