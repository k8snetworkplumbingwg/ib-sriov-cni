// Copyright 2025 ib-sriov-cni authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	rdmatypes "github.com/k8snetworkplumbingwg/rdma-cni/pkg/types"
)

var _ = Describe("Types", func() {
	Context("NetConf JSON Marshaling with custom MarshalJSON", func() {
		It("Should marshal all fields correctly including upstream DNS logic", func() {
			// Create a NetConf with all fields populated, including non-empty DNS
			netConf := &NetConf{
				PluginConf: cnitypes.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "qa-random-ipv4-nv-ipam-pool-2",
					Type:       "ib-sriov",
					DNS: cnitypes.DNS{
						Nameservers: []string{"8.8.8.8"},
						Domain:      "example.com",
					},
				},
				IbSriovNetConf: IbSriovNetConf{
					Master:              "ibp4s0f0",
					DeviceID:            "0000:04:00.7",
					VFID:                5,
					HostIFNames:         "ibp4s0f0v5",
					HostIFGUID:          "test-guid",
					ContIFNames:         "net1",
					PKey:                "0x8001",
					LinkState:           "enable",
					RdmaIso:             true,
					IBKubernetesEnabled: false,
					RdmaNetState: rdmatypes.RdmaNetState{
						Version:              "1.0",
						DeviceID:             "test-device",
						SandboxRdmaDevName:   "test-sandbox",
						ContainerRdmaDevName: "test-container",
					},
					RuntimeConfig: RuntimeConf{
						InfinibandGUID: "test-runtime-guid",
					},
					Args: struct {
						CNI map[string]string `json:"cni"`
					}{
						CNI: map[string]string{"test": "value"},
					},
				},
			}

			// Marshal to JSON
			jsonBytes, err := json.Marshal(netConf)
			Expect(err).NotTo(HaveOccurred())

			jsonStr := string(jsonBytes)

			// Verify all expected fields are present (both PluginConf and IbSriovNetConf)
			expectedFields := []string{
				// PluginConf fields
				`"cniVersion":"1.0.0"`,
				`"name":"qa-random-ipv4-nv-ipam-pool-2"`,
				`"type":"ib-sriov"`,
				`"dns"`,         // Should be present since DNS is non-empty
				`"nameservers"`, // Should be in DNS field

				// ib-sriov-specific fields
				`"Master":"ibp4s0f0"`,
				`"deviceID":"0000:04:00.7"`,
				`"VFID":5`,
				`"HostIFNames":"ibp4s0f0v5"`,
				`"HostIFGUID":"test-guid"`,
				`"ContIFNames":"net1"`,
				`"pkey":"0x8001"`,
				`"link_state":"enable"`,
				`"rdmaIsolation":true`,
				`"RdmaNetState"`,
				`"runtimeConfig"`,
				`"args"`,
			}

			for _, field := range expectedFields {
				Expect(strings.Contains(jsonStr, field)).To(BeTrue(),
					"Expected field %s not found in JSON output: %s", field, jsonStr)
			}
		})

		It("Should omit empty DNS field per upstream PluginConf logic", func() {
			// Test with empty DNS - should be omitted per upstream PluginConf.MarshalJSON logic
			netConf := &NetConf{
				PluginConf: cnitypes.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "test-config",
					Type:       "ib-sriov",
					// DNS field left empty - should be omitted by upstream logic
				},
				IbSriovNetConf: IbSriovNetConf{
					Master:   "ibp4s0f0",
					DeviceID: "0000:04:00.7",
					VFID:     5,
				},
			}

			jsonBytes, err := json.Marshal(netConf)
			Expect(err).NotTo(HaveOccurred())

			jsonStr := string(jsonBytes)

			// Verify required fields are present
			expectedFields := []string{
				`"cniVersion":"1.0.0"`,
				`"name":"test-config"`,
				`"type":"ib-sriov"`,
				`"Master":"ibp4s0f0"`,
				`"deviceID":"0000:04:00.7"`,
				`"VFID":5`,
			}

			for _, field := range expectedFields {
				Expect(strings.Contains(jsonStr, field)).To(BeTrue(),
					"Expected required field %s not found in JSON output: %s", field, jsonStr)
			}

			// Verify that "dns" field is omitted when DNS.IsEmpty() == true (upstream behavior)
			Expect(strings.Contains(jsonStr, `"dns"`)).To(BeFalse(),
				"dns field should be omitted when empty, but found in JSON output: %s", jsonStr)
		})

		It("Should properly round-trip marshal and unmarshal", func() {
			// Original NetConf
			original := &NetConf{
				PluginConf: cnitypes.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "test-config",
					Type:       "ib-sriov",
				},
				IbSriovNetConf: IbSriovNetConf{
					Master:      "ibp4s0f0",
					DeviceID:    "0000:04:00.7",
					VFID:        5,
					HostIFNames: "ibp4s0f0v5",
					ContIFNames: "net1",
					LinkState:   "enable",
				},
			}

			// Marshal and then unmarshal
			jsonBytes, err := json.Marshal(original)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaled NetConf
			err = json.Unmarshal(jsonBytes, &unmarshaled)
			Expect(err).NotTo(HaveOccurred())

			// Verify key fields were properly round-tripped
			Expect(unmarshaled.CNIVersion).To(Equal(original.CNIVersion))
			Expect(unmarshaled.Name).To(Equal(original.Name))
			Expect(unmarshaled.Type).To(Equal(original.Type))
			Expect(unmarshaled.Master).To(Equal(original.Master))
			Expect(unmarshaled.DeviceID).To(Equal(original.DeviceID))
			Expect(unmarshaled.VFID).To(Equal(original.VFID))
			Expect(unmarshaled.HostIFNames).To(Equal(original.HostIFNames))
			Expect(unmarshaled.ContIFNames).To(Equal(original.ContIFNames))
			Expect(unmarshaled.LinkState).To(Equal(original.LinkState))
		})

		It("Should fix the configuration save issue from broken version", func() {
			// This test verifies that we no longer get only basic CNI fields
			// The broken version only produced: {"cniVersion":"1.0.0","ipam":{},"name":"qa-random-ipv4-nv-ipam-pool-2","type":"ib-sriov"}
			netConf := &NetConf{
				PluginConf: cnitypes.PluginConf{
					CNIVersion: "1.0.0",
					Name:       "qa-random-ipv4-nv-ipam-pool-2",
					Type:       "ib-sriov",
				},
				IbSriovNetConf: IbSriovNetConf{
					Master:      "ibp4s0f0",
					DeviceID:    "0000:04:00.7",
					VFID:        5,
					HostIFNames: "ibp4s0f0v5",
					ContIFNames: "net1",
					LinkState:   "enable",
				},
			}

			jsonBytes, err := json.Marshal(netConf)
			Expect(err).NotTo(HaveOccurred())

			jsonStr := string(jsonBytes)

			// Verify we now get ALL the ib-sriov fields (not just basic CNI fields)
			requiredIbSriovFields := []string{
				`"Master":"ibp4s0f0"`,
				`"deviceID":"0000:04:00.7"`,
				`"VFID":5`,
				`"HostIFNames":"ibp4s0f0v5"`,
				`"ContIFNames":"net1"`,
				`"link_state":"enable"`,
			}

			for _, field := range requiredIbSriovFields {
				Expect(strings.Contains(jsonStr, field)).To(BeTrue(),
					"ib-sriov field %s should be present but not found in JSON output: %s", field, jsonStr)
			}

			// Verify it's not just the broken minimal output (should be much longer)
			Expect(len(jsonStr)).To(BeNumerically(">", 100),
				"JSON output should be much longer than the broken version, got: %s", jsonStr)
		})
	})
})
