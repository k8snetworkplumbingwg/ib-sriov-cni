package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

const publishingConfig = `{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"0000:03:00.0",` +
	`"ibKubernetesEnabled":true,"publishPfGuid":true,"vfioPciMode":true}`

// sandboxArgs is one sandbox's view of the same attachment: same Pod, same
// interface, same device, a different container ID.
func sandboxArgs(containerID string) *skel.CmdArgs {
	return &skel.CmdArgs{
		ContainerID: containerID, IfName: "ib1", Netns: "/var/run/netns/" + containerID,
		Args:      "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1;K8S_POD_UID=uid-1",
		StdinData: []byte(publishingConfig),
	}
}

// publishFrom publishes the attachment as the given sandbox and records it,
// the way an ADD does before it waits.
func publishFrom(t *testing.T, f *annotationClientFake, args *skel.CmdArgs, detail connectiondetails.Detail) {
	t.Helper()
	published, err := publishPodConnectionDetails(t.Context(), f, args, detail.DeviceID,
		func(string, string) (int, string, error) { return detail.DeviceIndex, detail.PfGUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	netConf := &localtypes.NetConf{}
	netConf.DeviceID = detail.DeviceID
	record := &publicationRecord{args: args, netConf: netConf}
	if err := record.record(published, nil); err != nil {
		t.Fatal(err)
	}
	// The write above was acknowledged: the record is a confirmed publication,
	// as the production path promotes it.
	record.promote()
}

// withdrawer is the DEL's withdrawal step against the fake client.
func withdrawer(f *annotationClientFake) publisher {
	return func(args *skel.CmdArgs, netConf *localtypes.NetConf) error {
		return removePodConnectionDetailsExpecting(context.Background(), f, args, netConf.DeviceID, netConf.PodUID, expectedMembers(netConf))
	}
}

// kubelet can tear down an earlier sandbox of a Pod after a later sandbox of
// the same Pod has published the same attachment again: the device plugin
// hands the Pod the same functions, so the late DEL names the same key. It
// must not take the live sandbox's record away.
func TestALateDELFromAnEarlierSandboxLeavesTheLaterSandboxsRecord(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	earlier, later := sandboxArgs("sandbox-a"), sandboxArgs("sandbox-b")
	publishFrom(t, f, earlier, mine)
	time.Sleep(2 * time.Millisecond)
	publishFrom(t, f, later, mine)
	patches := f.patches

	if _, err := withdrawConnectionDetails(t.Context(), earlier, withdrawer(f)); err != nil {
		t.Fatalf("late DEL of the earlier sandbox failed: %v", err)
	}
	if f.patches != patches {
		t.Fatalf("the earlier sandbox's DEL edited the Pod: %d patches", f.patches-patches)
	}
	checkEntry(t, f, mine, 1)

	// The sandbox that owns the record still withdraws it.
	if _, err := withdrawConnectionDetails(t.Context(), later, withdrawer(f)); err != nil {
		t.Fatal(err)
	}
	if details, _ := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation]); len(details) != 0 {
		t.Fatalf("the owning sandbox's DEL left the record: %+v", details)
	}
}

// A sandbox that never published owns nothing to withdraw, whatever UID the
// runtime passes; its DEL succeeds and edits nothing.
func TestADELFromASandboxThatNeverPublishedWithdrawsNothing(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	publishFrom(t, f, sandboxArgs("sandbox-a"), mine)
	patches := f.patches

	if _, err := withdrawConnectionDetails(t.Context(), sandboxArgs("sandbox-never"), withdrawer(f)); err != nil {
		t.Fatal(err)
	}
	if f.patches != patches {
		t.Fatalf("a sandbox with no record edited the Pod: %d patches", f.patches-patches)
	}
	checkEntry(t, f, mine, 1)
}

// Multus injects the device the runtime allocated into both ADD and DEL. A
// DEL that carries none has nothing to withdraw: it says so at error level,
// because on a healthy node that never happens, and still succeeds so the
// runtime can finish the teardown.
func TestADELWithoutADeviceLogsAnErrorAndSucceeds(t *testing.T) {
	cacheDirAt(t)
	var logged bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	klog.LogToStderr(false)
	klog.SetOutput(&logged)
	args := sandboxArgs("sandbox-a")
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","ibKubernetesEnabled":true,"publishPfGuid":true,"vfioPciMode":true}`)
	called := false
	_, err := withdrawConnectionDetails(t.Context(), args, func(*skel.CmdArgs, *localtypes.NetConf) error { called = true; return nil })
	klog.Flush()
	if err != nil || called {
		t.Fatalf("DEL without a device: err=%v withdraw called=%v; want success and no withdrawal", err, called)
	}
	// klog prefixes each line with its severity letter and the date, so the
	// message must sit on a line that starts with E.
	atErrorLevel := false
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.HasPrefix(line, "E") && strings.Contains(line, "no deviceID") {
			atErrorLevel = true
		}
	}
	if !atErrorLevel {
		t.Fatalf("the missing device was not logged as an error: %q", logged.String())
	}
}
