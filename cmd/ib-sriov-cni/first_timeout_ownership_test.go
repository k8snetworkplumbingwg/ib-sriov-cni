package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// cacheDirAt points the node-local state this process writes, the CNI
// configuration cache and the lock directory, at directories the test owns.
func cacheDirAt(t *testing.T) {
	t.Helper()
	previousCache, previousLock := config.DefaultCNIDir, config.CniFileLockDir
	config.DefaultCNIDir, config.CniFileLockDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { config.DefaultCNIDir, config.CniFileLockDir = previousCache, previousLock })
}

// testPF is the one function the composed tests attach.
const testPF = "0000:03:00.0"

// vfioPFAt fakes the sysfs entries that make testPF a physical function bound
// to vfio-pci, which is what routes an ADD through publication.
func vfioPFAt(t *testing.T) {
	t.Helper()
	deviceID := testPF
	previous := utils.SysBusPci
	utils.SysBusPci = t.TempDir()
	t.Cleanup(func() { utils.SysBusPci = previous })
	if err := os.MkdirAll(filepath.Join(utils.SysBusPci, deviceID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/not-a-real-driver/vfio-pci", filepath.Join(utils.SysBusPci, deviceID, "driver")); err != nil {
		t.Fatal(err)
	}
}

// A first ADD that publishes and then times out is rolled back by a DEL that,
// on an older runtime, carries no K8S_POD_UID. The ownership the ADD observed
// has to be on disk before the wait begins, or that DEL has no evidence and
// the published member outlives the sandbox. Exercised through the shared ADD
// and DEL entry points, with only the API and the hardware faked.
func TestOwnershipIsRecordedBeforeTheWaitSoAFirstTimeoutIsWithdrawn(t *testing.T) {
	for name, withUID := range map[string]bool{"without CNI UID": false, "with CNI UID": true} {
		t.Run(name, func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			pods, client := startNodeCache(t, concurrencyPod("vm-a", "ib1"))
			reader := nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(client)}
			device := func(string, string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil }
			// The rollback DEL names the sandbox's netns as the runtime does;
			// by then the sandbox is gone, which DEL treats as done.
			args := &skel.CmdArgs{
				ContainerID: "fresh-sandbox", IfName: "ib1", Netns: filepath.Join(t.TempDir(), "gone"),
				Args: "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-a",
				StdinData: []byte(`{"name":"net-a","type":"ib-sriov","cniVersion":"0.3.1","deviceID":"0000:03:00.0",` +
					`"ibKubernetesEnabled":true,"publishPfGuid":true,"vfioPciMode":true,"readinessTimeoutSeconds":1}`),
			}
			if withUID {
				args.Args += ";K8S_POD_UID=uid-vm-a"
			}
			_, _, addErr := addNetwork(t.Context(), args, invocation{kube: daemonPublisher(t.Context(), reader, device)})
			if addErr == nil || !strings.Contains(addErr.Error(), "context deadline exceeded") {
				t.Fatalf("expected the readiness timeout, got %v", addErr)
			}
			if got := recordedPodUID(args); got != "uid-vm-a" {
				t.Fatalf("ownership not recorded before the wait: recorded UID %q", got)
			}
			live, err := client.CoreV1().Pods("ns1").Get(t.Context(), "vm-a", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if details, err := connectiondetails.Parse(live.Annotations[connectiondetails.Annotation]); err != nil || len(details) != 1 {
				t.Fatalf("ADD did not publish before the timeout: %v %v", details, err)
			}
			if err := delNetwork(t.Context(), args, invocation{kube: daemonWithdrawer(t.Context(), reader)}); err != nil {
				t.Fatal(err)
			}
			live, err = client.CoreV1().Pods("ns1").Get(t.Context(), "vm-a", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			details, err := connectiondetails.Parse(live.Annotations[connectiondetails.Annotation])
			if err != nil || len(details) != 0 {
				t.Fatalf("the rollback DEL left the published member behind: %v %v", details, err)
			}
			if entries, _ := os.ReadDir(config.DefaultCNIDir); len(entries) != 0 {
				t.Fatalf("DEL left the cached configuration behind: %v", entries)
			}
		})
	}
}
