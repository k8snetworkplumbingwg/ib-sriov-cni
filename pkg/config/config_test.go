package config

import (
	"os"
	"path/filepath"

	"github.com/containernetworking/cni/pkg/skel"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Config", func() {
	Context("Checking LoadConf function", func() {
		It("Assuming correct config file - existing DeviceID", func() {
			conf := []byte(`{
        "name": "mynet",
        "type": "ib-sriov",
        "deviceID": "0000:af:06.1",
        "vf": 0,
        "ipam": {
            "type": "host-local",
            "subnet": "10.55.206.0/26",
            "routes": [
                { "dst": "0.0.0.0/0" }
            ],
            "gateway": "10.55.206.1"
        }
                        }`)
			_, err := LoadConf(conf)
			Expect(err).NotTo(HaveOccurred())
		})
		It("Assuming incorrect config file - broken json", func() {
			conf := []byte(`{
        "name": "mynet"
		"type": "ib-sriov",
		"deviceID": "0000:af:06.1",
        "vf": 0,
        "ipam": {
            "type": "host-local",
            "subnet": "10.55.206.0/26",
            "routes": [
                { "dst": "0.0.0.0/0" }
            ],
            "gateway": "10.55.206.1"
        }
                        }`)
			_, err := LoadConf(conf)
			Expect(err).To(HaveOccurred())
		})
		It("Rejects an out-of-range PCI device number", func() {
			_, err := LoadConf([]byte(`{"deviceID":"0000:00:20.0"}`))
			Expect(err).To(MatchError(ContainSubstring("invalid deviceID")))
		})
		It("Normalizes an uppercase PCI address", func() {
			netConf, err := LoadConf([]byte(`{"deviceID":"0000:AF:06.1"}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(netConf.DeviceID).To(Equal("0000:af:06.1"))
		})
	})
	Context("Checking LoadConfFromCache function", func() {
		var cacheDir string

		BeforeEach(func() {
			cacheDir = GinkgoT().TempDir()
			previousCNIDir := DefaultCNIDir
			DefaultCNIDir = cacheDir
			DeferCleanup(func() {
				DefaultCNIDir = previousCNIDir
			})
		})

		It("Revalidates a cached device ID", func() {
			cachePath := filepath.Join(cacheDir, "container-net1")
			Expect(os.WriteFile(cachePath, []byte(`{"deviceID":"0000:00:20.0"}`), 0600)).To(Succeed())

			_, _, err := LoadConfFromCache(&skel.CmdArgs{ContainerID: "container", IfName: "net1"})
			Expect(err).To(MatchError(ContainSubstring("failed to validate cached NetConf")))
		})

		It("Revalidates cached link state", func() {
			cachePath := filepath.Join(cacheDir, "container-net1")
			Expect(os.WriteFile(cachePath, []byte(`{"deviceID":"0000:af:06.1","link_state":"invalid"}`), 0600)).To(Succeed())

			_, _, err := LoadConfFromCache(&skel.CmdArgs{ContainerID: "container", IfName: "net1"})
			Expect(err).To(MatchError(ContainSubstring("failed to validate cached NetConf")))
		})

		It("Rejects path separators before reading the cache", func() {
			_, _, err := LoadConfFromCache(&skel.CmdArgs{ContainerID: "../container", IfName: "net1"})
			Expect(err).To(MatchError(ContainSubstring("path separator")))
		})
	})
	Context("Checking getVfInfo function", func() {
		It("Assuming existing PF", func() {
			_, _, err := getVfInfo("0000:af:06.0")
			Expect(err).NotTo(HaveOccurred())
		})
		It("Assuming not existing PF", func() {
			_, _, err := getVfInfo("0000:af:07.0")
			Expect(err).To(HaveOccurred())
		})
	})
})
