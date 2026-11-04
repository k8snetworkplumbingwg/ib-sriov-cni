package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("LoadConf and the cached Pod UID", func() {
	It("ignores a podUID or publishedAt carried by the network configuration", func() {
		netConf, err := LoadConf([]byte(`{"name":"n","type":"ib-sriov","deviceID":"0000:af:06.1","podUID":"injected","publishedAt":"2026-01-01T00:00:00Z"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(netConf.PodUID).To(BeEmpty())
		Expect(netConf.PublishedAt).To(BeEmpty())
	})
})
