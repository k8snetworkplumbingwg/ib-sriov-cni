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

var _ = Describe("IB SR-IOV thin entrypoint testing", func() {
	It("should always pass basic test", func() {
		a := 10
		Expect(a).To(Equal(10))
	})

	It("should run verifyFileExists() with valid environment", func() {
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

		// Test verifyFileExists with valid paths
		err = (&Options{
			CNIBinDir:         cniBinDir,
			IBSriovCNIBinFile: ibSriovBinFile,
		}).verifyFileExists()
		Expect(err).NotTo(HaveOccurred())
	})

	It("should fail verifyFileExists() with missing CNI bin directory", func() {
		// Create temporary directory
		tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cniBinDir := fmt.Sprintf("%s/non_existent_cni_bin_dir", tmpDir)
		ibSriovBinFile := fmt.Sprintf("%s/ib-sriov", tmpDir)

		// Create IB SR-IOV binary file but not CNI bin directory
		Expect(os.WriteFile(ibSriovBinFile, []byte("dummy-binary-content"), 0755)).To(Succeed())

		// Test verifyFileExists with missing CNI bin directory
		err = (&Options{
			CNIBinDir:         cniBinDir,
			IBSriovCNIBinFile: ibSriovBinFile,
		}).verifyFileExists()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("CNI bin directory"))
		Expect(err.Error()).To(ContainSubstring("does not exist"))
	})

	It("should fail verifyFileExists() with missing IB SR-IOV binary file", func() {
		// Create temporary directory
		tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
		ibSriovBinFile := fmt.Sprintf("%s/non_existent_ib_sriov", tmpDir)

		// Create CNI bin directory but not IB SR-IOV binary file
		Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

		// Test verifyFileExists with missing IB SR-IOV binary file
		err = (&Options{
			CNIBinDir:         cniBinDir,
			IBSriovCNIBinFile: ibSriovBinFile,
		}).verifyFileExists()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("IB SR-IOV CNI binary file"))
		Expect(err.Error()).To(ContainSubstring("does not exist"))
	})

	It("should run copyBinary() successfully", func() {
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
		Expect(fileInfo.Mode() & 0755).To(Equal(os.FileMode(0755)))
	})

	It("should fail copyBinary() with missing source file", func() {
		// Create temporary directory
		tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cniBinDir := fmt.Sprintf("%s/cni_bin_dir", tmpDir)
		ibSriovBinFile := fmt.Sprintf("%s/non_existent_ib_sriov", tmpDir)

		// Create CNI bin directory but not source file
		Expect(os.Mkdir(cniBinDir, 0755)).To(Succeed())

		// Test copyBinary with missing source file
		err = (&Options{
			CNIBinDir:         cniBinDir,
			IBSriovCNIBinFile: ibSriovBinFile,
		}).copyBinary()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to copy binary"))
	})

	It("should fail copyBinary() with missing destination directory", func() {
		// Create temporary directory
		tmpDir, err := os.MkdirTemp("", "ib_sriov_thin_entrypoint_tmp")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(tmpDir)

		cniBinDir := fmt.Sprintf("%s/non_existent_cni_bin_dir", tmpDir)
		ibSriovBinFile := fmt.Sprintf("%s/ib-sriov-src", tmpDir)

		// Create source file but not destination directory
		testContent := []byte("test-ib-sriov-binary-content")
		Expect(os.WriteFile(ibSriovBinFile, testContent, 0755)).To(Succeed())

		// Test copyBinary with missing destination directory
		err = (&Options{
			CNIBinDir:         cniBinDir,
			IBSriovCNIBinFile: ibSriovBinFile,
		}).copyBinary()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to copy binary"))
	})

	It("should handle addFlags() correctly", func() {
		// Test that addFlags doesn't panic
		opt := Options{}
		Expect(func() { opt.addFlags() }).NotTo(Panic())

		// The behavior of pflag is that it may populate struct fields
		// with default values when StringVar is called, so we just
		// verify the function completes without error
	})

	It("should create Options struct with correct default values", func() {
		opt := Options{}

		// Verify struct fields exist and have correct zero values
		Expect(opt.CNIBinDir).To(Equal(""))
		Expect(opt.IBSriovCNIBinFile).To(Equal(""))
	})

	It("should handle file permissions correctly in copyBinary()", func() {
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

		// Test copyBinary
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

		// Test copyBinary with empty file
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
