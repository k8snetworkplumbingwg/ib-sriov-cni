package utils

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

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
	Context("Checking IsVirtualFunction function", func() {
		It("Assuming VF device (has physfn)", func() {
			// This test assumes 0000:af:06.0 is a VF with physfn symlink
			result, err := IsVirtualFunction("0000:af:06.0")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for valid PCI address")
			Expect(result).To(Equal(true), "VF device should return true")
		})
		It("Assuming PF device (no physfn)", func() {
			// Test with the actual PF device 0000:af:00.1 (ib0) from fixture
			result, err := IsVirtualFunction("0000:af:00.1")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for valid PCI address")
			Expect(result).To(Equal(false), "PF device should return false")
		})
		It("Assuming VFIO VF device (has physfn)", func() {
			// Test with VFIO VF 0000:af:06.1 - should still be detected as VF
			result, err := IsVirtualFunction("0000:af:06.1")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for valid PCI address")
			Expect(result).To(Equal(true), "VFIO VF device should still return true")
		})
		It("Assuming non-existing device", func() {
			// This should return false and no error for non-existing device
			result, err := IsVirtualFunction("0000:ff:ff.f")
			Expect(err).NotTo(HaveOccurred(), "Should not return error for non-existing device")
			Expect(result).To(Equal(false), "Non-existing device should return false")
		})
	})
	Context("Checking ValidatePathComponents function", func() {
		It("Should accept clean component values", func() {
			err := ValidatePathComponents(
				PathComponent{Name: "container ID", Value: "abc123"},
				PathComponent{Name: "interface name", Value: "eth0"},
			)
			Expect(err).NotTo(HaveOccurred())
		})
		It("Should reject component with forward slash", func() {
			err := ValidatePathComponents(
				PathComponent{Name: "container ID", Value: "abc/def"},
			)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("container ID"))
			Expect(err.Error()).To(ContainSubstring("path separator"))
		})
		It("Should reject component with backslash", func() {
			err := ValidatePathComponents(
				PathComponent{Name: "interface name", Value: `eth\0`},
			)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("interface name"))
		})
		It("Should reject component with path traversal pattern", func() {
			err := ValidatePathComponents(
				PathComponent{Name: "container ID", Value: "../../tmp"},
			)
			Expect(err).To(HaveOccurred())
		})
		It("Should reject only the first invalid component", func() {
			err := ValidatePathComponents(
				PathComponent{Name: "container ID", Value: "a/b"},
				PathComponent{Name: "interface name", Value: "c/d"},
			)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("container ID"))
		})
		It("Should accept empty component value", func() {
			err := ValidatePathComponents(
				PathComponent{Name: "container ID", Value: ""},
			)
			Expect(err).NotTo(HaveOccurred())
		})
	})
	Context("SaveNetConf input boundaries", func() {
		It("rejects separators in either field before serialization or directory creation", func() {
			for _, pair := range [][2]string{
				{"../container", "net1"}, {`container\child`, "net1"},
				{"container", "../net1"}, {"container", `net\1`},
			} {
				dir := filepath.Join(GinkgoT().TempDir(), "absent")
				err := SaveNetConf(pair[0], dir, pair[1], make(chan int))
				Expect(err).To(MatchError(ContainSubstring("path separator")))
				_, err = os.Stat(dir)
				Expect(os.IsNotExist(err)).To(BeTrue())
			}
		})
	})
	Context("Checking scratch NetConf file handling", func() {
		It("Should preserve regular cache I/O with symlink protection enabled", func() {
			tempDir := GinkgoT().TempDir()
			Expect(SaveNetConf("container", tempDir, "net1", map[string]string{"name": "test"})).To(Succeed())

			contents, err := ReadScratchNetConf(filepath.Join(tempDir, "container-net1"))
			Expect(err).NotTo(HaveOccurred())
			Expect(contents).To(MatchJSON(`{"name":"test"}`))
		})

		It("Should refuse to read through a symlink", func() {
			tempDir := GinkgoT().TempDir()
			targetPath := filepath.Join(tempDir, "target")
			cachePath := filepath.Join(tempDir, "cache")
			Expect(os.WriteFile(targetPath, []byte("sensitive"), OwnerReadWriteAttrs)).To(Succeed())
			Expect(os.Symlink(targetPath, cachePath)).To(Succeed())

			contents, err := ReadScratchNetConf(cachePath)
			Expect(err).To(HaveOccurred())
			Expect(contents).To(BeNil())
		})

		It("Should refuse to write through a symlink", func() {
			tempDir := GinkgoT().TempDir()
			targetPath := filepath.Join(tempDir, "target")
			cachePath := filepath.Join(tempDir, "container-net1")
			Expect(os.WriteFile(targetPath, []byte("sensitive"), OwnerReadWriteAttrs)).To(Succeed())
			Expect(os.Symlink(targetPath, cachePath)).To(Succeed())

			err := SaveNetConf("container", tempDir, "net1", map[string]string{"name": "test"})
			Expect(err).To(HaveOccurred())

			contents, err := os.ReadFile(targetPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(contents).To(Equal([]byte("sensitive")))
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

type failingScratchFile struct {
	*os.File
	stage string
	err   error
}

func (f failingScratchFile) Write(data []byte) (int, error) {
	if f.stage == "write" {
		_, _ = f.File.Write(data[:1])
		return 1, f.err
	}
	if f.stage == "short write" {
		return 0, nil
	}
	return f.File.Write(data)
}

func (f failingScratchFile) Close() error {
	err := f.File.Close()
	if f.stage == "close" {
		return f.err
	}
	return err
}

func TestAtomicScratchReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "container-net1")
	for _, data := range []string{`{"old":true,"long":"configuration"}`, `{"new":true}`} {
		if err := saveScratchNetConf("container-net1", dir, []byte(data)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadScratchNetConf(path)
		if err != nil || string(got) != data {
			t.Fatalf("cache = %q, error = %v", got, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("permissions = %o", info.Mode().Perm())
		}
	}
}

func TestScratchFailurePreservesCache(t *testing.T) {
	for _, stage := range []string{"create", "write", "short write", "close", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cache")
			old := []byte(`{"old":true}`)
			if err := os.WriteFile(path, old, 0600); err != nil {
				t.Fatal(err)
			}
			want := errors.New("injected failure")
			if stage == "short write" {
				want = io.ErrShortWrite
			}
			create := func(dir string) (scratchFile, error) {
				if stage == "create" {
					return nil, want
				}
				file, err := os.CreateTemp(dir, ".netconf-*")
				if err != nil {
					return nil, err
				}
				return failingScratchFile{File: file, stage: stage, err: want}, nil
			}
			rename := func(from, to string) error {
				if stage == "rename" {
					return want
				}
				return os.Rename(from, to)
			}
			err := writeScratchNetConf("cache", dir, []byte(`{"new":true}`), create, rename)
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, old) {
				t.Fatalf("old cache changed: %q, error = %v", got, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "cache" {
				t.Fatalf("temporary file remains: %v, error = %v", entries, err)
			}
		})
	}
}

func TestScratchMissingFileError(t *testing.T) {
	_, err := ReadScratchNetConf(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error does not preserve missing-file identity: %v", err)
	}
}
