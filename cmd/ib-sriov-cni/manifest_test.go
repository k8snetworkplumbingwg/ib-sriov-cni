package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The image is built for amd64 and arm64 and the handshake was validated on
// arm64 nodes, so the reference manifest places the DaemonSet by affinity on
// both architectures and carries no architecture in its name; the stock
// manifest's amd64-only selector would exclude the validated platform.
func TestReadinessManifestPlacesOnBothArchitectures(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "deployment", "readiness", "ib-sriov-cni-standalone.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	if regexp.MustCompile(`(?m)^\s*kubernetes\.io/arch:\s*amd64\s*$`).MatchString(text) {
		t.Fatal("the manifest selects amd64 only")
	}
	if strings.Contains(text, "ds-amd64") {
		t.Fatal("the manifest names an architecture in the DaemonSet name")
	}
	if !strings.Contains(text, "key: kubernetes.io/arch") {
		t.Fatal("the manifest has no architecture affinity")
	}
	for _, arch := range []string{"- amd64", "- arm64"} {
		if !strings.Contains(text, arch) {
			t.Fatalf("the manifest does not place the DaemonSet on %s", strings.TrimPrefix(arch, "- "))
		}
	}
}

// The installer copies one binary and writes one kubeconfig. Root in the
// container is enough to write those host paths, so it runs without
// privileges and mounts only the directory it writes the kubeconfig to, not
// every CNI configuration on the node.
func TestReadinessManifestInstallerIsUnprivilegedAndMountsOnlyItsDirectory(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "deployment", "readiness", "ib-sriov-cni-standalone.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	if regexp.MustCompile(`(?m)^\s*privileged:\s*true\s*$`).MatchString(text) {
		t.Fatal("the installer runs privileged; copying a binary and writing a kubeconfig need root only")
	}
	for _, want := range []string{"allowPrivilegeEscalation: false", "readOnlyRootFilesystem: true", `drop: ["ALL"]`, "path: /etc/cni/net.d/ib-sriov-cni.d\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the installer manifest lacks %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s*path:\s*/etc/cni/net\.d\s*$`).MatchString(text) {
		t.Fatal("the installer mounts all of /etc/cni/net.d, every CNI configuration on the node")
	}
}
