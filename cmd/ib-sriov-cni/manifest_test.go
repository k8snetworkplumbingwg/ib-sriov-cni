package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The daemon opens the network namespace path each request names through the
// host's netns directories, which the manifest mounts; it never resolves a
// namespace by process. The DaemonSet therefore mounts those directories and
// does not share the host PID namespace, which would expose every host process
// to a privileged container that can patch Pods.
func TestDaemonManifestMountsNetnsDirectoriesInsteadOfSharingHostPID(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "deployment", "readiness", "ib-sriov-cni-daemon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	if hostPID := regexp.MustCompile(`(?m)^\s*hostPID:\s*true\s*$`); hostPID.MatchString(text) {
		t.Fatal("the daemon manifest shares the host PID namespace; it only needs the mounted netns directories")
	}
	for _, dir := range []string{"/run/netns", "/var/run/netns"} {
		if !strings.Contains(text, "mountPath: "+dir) {
			t.Fatalf("the daemon manifest does not mount %s, which it needs to open netns paths without host PID access", dir)
		}
	}
}

// The image is built for amd64 and arm64 and the handshake was validated on
// arm64 nodes, so the reference manifests place the DaemonSet by affinity on
// both architectures and carry no architecture in their name; the stock
// manifest's amd64-only selector would exclude the validated platform.
func TestReadinessManifestsPlaceOnBothArchitectures(t *testing.T) {
	for _, name := range []string{"ib-sriov-cni-daemon.yaml", "ib-sriov-cni-standalone.yaml"} {
		manifest, err := os.ReadFile(filepath.Join("..", "..", "deployment", "readiness", name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(manifest)
		if regexp.MustCompile(`(?m)^\s*kubernetes\.io/arch:\s*amd64\s*$`).MatchString(text) {
			t.Fatalf("%s selects amd64 only", name)
		}
		if strings.Contains(text, "ds-amd64") {
			t.Fatalf("%s names an architecture in the DaemonSet name", name)
		}
		if !strings.Contains(text, "key: kubernetes.io/arch") {
			t.Fatalf("%s has no architecture affinity", name)
		}
		for _, arch := range []string{"- amd64", "- arm64"} {
			if !strings.Contains(text, arch) {
				t.Fatalf("%s does not place the DaemonSet on %s", name, strings.TrimPrefix(arch, "- "))
			}
		}
	}
}

// /var/run is not a symlink to /run on every host, so each netns directory the
// daemon opens paths under is mounted from the same host path, not from one
// shared hostPath volume.
func TestDaemonManifestMountsEachNetnsDirectoryFromItsOwnHostPath(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "deployment", "readiness", "ib-sriov-cni-daemon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	for _, dir := range []string{"/run/netns", "/var/run/netns"} {
		if !strings.Contains(text, "path: "+dir+"\n") {
			t.Fatalf("the daemon manifest has no hostPath volume for %s", dir)
		}
	}
}
