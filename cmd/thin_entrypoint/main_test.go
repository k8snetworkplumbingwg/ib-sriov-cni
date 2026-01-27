package main

// disable dot-imports only for testing
//revive:disable:dot-imports
import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:golint
	. "github.com/onsi/gomega"    //nolint:golint
)

func TestThinEntrypoint(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ib-sriov thin_entrypoint")
}

var _ = Describe("IB SR-IOV thin entrypoint", func() {
	Describe("verifyPaths", func() {
		It("should succeed with valid environment", func() {
			// Create temporary directory and files
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov", tmpDir)

			// Create CNI bin directory
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Create IB SR-IOV binary file
			Expect(os.WriteFile(ibSriovBinFile, []byte("dummy-binary-content"), 0755)).To(Succeed())

			// Valid paths should not error
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).verifyPaths()
			Expect(err).NotTo(HaveOccurred())
		})

		It("should fail with missing CNI bin directory", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/non_existent_cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov", tmpDir)

			// Create IB SR-IOV binary file but not CNI bin directory
			Expect(os.WriteFile(ibSriovBinFile, []byte("dummy-binary-content"), 0755)).To(Succeed())

			// Missing CNI bin directory should error
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).verifyPaths()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("CNI bin directory"))
			Expect(err.Error()).To(ContainSubstring("does not exist"))
		})

		It("should fail with missing IB SR-IOV binary file", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/non_existent_ib_sriov", tmpDir)

			// Create CNI bin directory but not IB SR-IOV binary file
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Missing IB SR-IOV binary file should error
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).verifyPaths()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("IB SR-IOV CNI binary file"))
			Expect(err.Error()).To(ContainSubstring("does not exist"))
		})

		It("should fail when IB SR-IOV binary file is a directory", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov-dir", tmpDir)

			// Create CNI bin directory
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Create a directory instead of a file for the IB SR-IOV binary
			Expect(os.Mkdir(ibSriovBinFile, 0755)).To(Succeed())

			// Should error because binary file is a directory
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).verifyPaths()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is not a regular file"))
		})
	})

	Describe("copyBinary", func() {
		It("should succeed with valid paths", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov-src", tmpDir)

			// Create CNI bin directory
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Create source IB SR-IOV binary file with test content
			testContent := []byte("test-ib-sriov-binary-content")
			Expect(os.WriteFile(ibSriovBinFile, testContent, 0755)).To(Succeed())

			// Test copyBinary
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).copyBinary()
			Expect(err).NotTo(HaveOccurred())

			// Verify the binary was copied to the correct location
			dstFile := filepath.Join(cniBinDir, "ib-sriov")
			Expect(dstFile).To(BeAnExistingFile())

			// Verify the content matches
			copiedContent, err := os.ReadFile(dstFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(copiedContent).To(Equal(testContent))

			// Verify the file permissions
			fileInfo, err := os.Stat(dstFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(fileInfo.Mode().Perm()).To(Equal(os.FileMode(0755)))
		})

		It("should fail with missing source file", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/non_existent_ib_sriov", tmpDir)

			// Create CNI bin directory but not source file
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Missing source file should error
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).copyBinary()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to copy binary"))
		})

		It("should fail with missing destination directory", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/non_existent_cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov-src", tmpDir)

			// Create source file but not destination directory
			testContent := []byte("test-ib-sriov-binary-content")
			Expect(os.WriteFile(ibSriovBinFile, testContent, 0755)).To(Succeed())

			// Missing destination directory should error
			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).copyBinary()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to copy binary"))
		})

		It("should handle file permissions correctly", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov-src", tmpDir)

			// Create CNI bin directory
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Create source file with specific permissions
			testContent := []byte("test-ib-sriov-binary-content")
			Expect(os.WriteFile(ibSriovBinFile, testContent, 0644)).To(Succeed())

			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).copyBinary()
			Expect(err).NotTo(HaveOccurred())

			// Verify the destination file has the same permissions as source
			dstFile := filepath.Join(cniBinDir, "ib-sriov")
			srcInfo, err := os.Stat(ibSriovBinFile)
			Expect(err).NotTo(HaveOccurred())

			dstInfo, err := os.Stat(dstFile)
			Expect(err).NotTo(HaveOccurred())

			Expect(dstInfo.Mode()).To(Equal(srcInfo.Mode()))
		})

		It("should handle empty file copying correctly", func() {
			// Create temporary directory
			tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
			ibSriovBinFile := fmt.Sprintf("%s/ib-sriov-src", tmpDir)

			// Create CNI bin directory
			Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

			// Create empty source file
			Expect(os.WriteFile(ibSriovBinFile, []byte{}, 0755)).To(Succeed())

			err = (&Options{
				CNIBinDir:         cniBinDir,
				IBSriovCNIBinFile: ibSriovBinFile,
			}).copyBinary()
			Expect(err).NotTo(HaveOccurred())

			// Verify the empty file was copied
			dstFile := filepath.Join(cniBinDir, "ib-sriov")
			Expect(dstFile).To(BeAnExistingFile())

			copiedContent, err := os.ReadFile(dstFile)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(copiedContent)).To(Equal(0))
		})
	})

	Describe("Options", func() {
		It("should handle addFlags correctly", func() {
			// Test that addFlags doesn't panic
			opt := Options{}
			Expect(func() { opt.addFlags() }).NotTo(Panic())

			// The behavior of flag is that it may populate struct fields
			// with default values when StringVar is called, so we just
			// verify the function completes without error
		})
	})
})
