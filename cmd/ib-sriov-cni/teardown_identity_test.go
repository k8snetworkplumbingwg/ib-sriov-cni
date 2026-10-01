package main

import (
	"testing"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// A runtime that passes no K8S_POD_UID leaves DEL with one other witness: the
// UID the ADD recorded with its cached configuration.
func TestUIDLessDELWithdrawsWithTheUIDRecordedAtADD(t *testing.T) {
	f, args := newAnnotationTest(t)
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1"
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	published, err := publishPodConnectionDetails(t.Context(), f, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := removePodConnectionDetails(t.Context(), f, args, mine.DeviceID, published.UID); err != nil {
		t.Fatal(err)
	}
	details, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 0 || f.patches != 2 {
		t.Fatalf("own entry not withdrawn with the recorded UID: details=%+v patches=%d", details, f.patches)
	}
}

// The recorded UID fences exactly like one from CNI_ARGS: a same-name
// replacement that now holds the address keeps its allocation.
func TestUIDLessDELWithRecordedUIDLeavesAReplacementAlone(t *testing.T) {
	f, args := newAnnotationTest(t)
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1"
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	published, err := publishPodConnectionDetails(t.Context(), f, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	replacement := mine
	replacement.PfGUID = "90:e3:17:03:00:fb:91:99"
	f.pod = f.pod.DeepCopy()
	f.pod.UID = "uid-2"
	f.pod.ResourceVersion = "100"
	f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t,
		map[string]connectiondetails.Detail{replacement.Key(): replacement})
	patches := f.patches
	if err := removePodConnectionDetails(t.Context(), f, args, mine.DeviceID, published.UID); err != nil {
		t.Fatal(err)
	}
	details, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if got, present := details[replacement.Key()]; !present || got != replacement || f.patches != patches {
		t.Fatalf("replacement edited by a DEL fenced to %s: details=%+v patches=%d", published.UID, details, f.patches)
	}
}

// With neither a UID in CNI_ARGS nor a record from ADD there is no evidence
// which Pod the attachment belonged to; DEL succeeds and edits nothing.
func TestUIDLessDELWithoutEvidenceEditsNothing(t *testing.T) {
	f, args := newAnnotationTest(t)
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1"
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	if _, err := publishPodConnectionDetails(t.Context(), f, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
		t.Fatal(err)
	}
	if err := removePodConnectionDetails(t.Context(), f, args, mine.DeviceID, ""); err != nil {
		t.Fatalf("a DEL without evidence must still succeed: %v", err)
	}
	details, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if _, present := details[mine.Key()]; !present || f.patches != 1 {
		t.Fatalf("a DEL without evidence edited the Pod: details=%+v patches=%d", details, f.patches)
	}
}

// The record DEL falls back on is the configuration ADD cached for the same
// container and interface; nothing else on the node is consulted.
func TestRecordedPodUIDComesFromTheCachedConfiguration(t *testing.T) {
	previous := config.DefaultCNIDir
	config.DefaultCNIDir = t.TempDir()
	t.Cleanup(func() { config.DefaultCNIDir = previous })
	conf := &localtypes.NetConf{}
	conf.Name = "net-a"
	conf.Type = "ib-sriov"
	conf.DeviceID = "0000:03:00.0"
	conf.PodUID = "uid-1"
	if err := utils.SaveNetConf("cid-1", config.DefaultCNIDir, "ib1", conf); err != nil {
		t.Fatal(err)
	}
	if got := recordedPodUID(&skel.CmdArgs{ContainerID: "cid-1", IfName: "ib1"}); got != "uid-1" {
		t.Fatalf("recorded UID = %q, want uid-1", got)
	}
	if got := recordedPodUID(&skel.CmdArgs{ContainerID: "cid-2", IfName: "ib1"}); got != "" {
		t.Fatalf("an attachment without a record reported %q", got)
	}
}

// The daemon's publisher records the UID it published to on the configuration
// the ADD goes on to cache, which is where a later UID-less DEL reads it back.
func TestDaemonPublisherRecordsThePodUIDForTeardown(t *testing.T) {
	cacheDirAt(t)
	pods, client := startNodeCache(t, concurrencyPod("vm-a", "ib1"))
	reader := nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(client)}
	device := func(string, string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil }
	netConf := waitingNetConf("0000:03:00.0")
	done := make(chan error, 1)
	go func() {
		done <- daemonPublisher(t.Context(), reader, device)(attachmentArgs("vm-a", "ib1"), netConf)
	}()
	markReadyWhenPublished(t, client, "vm-a", "0000:03:00.0")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if netConf.PodUID != "uid-vm-a" {
		t.Fatalf("recorded PodUID = %q, want the published Pod's uid-vm-a", netConf.PodUID)
	}
	if got := recordedPodUID(attachmentArgs("vm-a", "ib1")); got != "uid-vm-a" {
		t.Fatalf("the cached configuration does not carry the UID: %q", got)
	}
}
