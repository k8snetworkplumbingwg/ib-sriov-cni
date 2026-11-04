package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// completeADD runs the shared ADD entry point for a PF passthrough through
// publication, the readiness wait and the final configuration save, with the
// API and the hardware faked. The sandbox's netns is this process's own.
func completeADD(t *testing.T, api *annotationClientFake, args *skel.CmdArgs, mine connectiondetails.Detail) {
	t.Helper()
	args.Netns = "/proc/self/ns/net"
	api.pod.Annotations[connectiondetails.StatusAnnotation] = `{"` + mine.Key() + `":{"Status":"Ready"}}`
	publish := func(args *skel.CmdArgs, conf *localtypes.NetConf) error {
		published, err := publishRecorded(t.Context(), api, args, conf,
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
		if err != nil {
			return err
		}
		return awaitConnectionReady(context.Background(), api, args, conf, published)
	}
	result, version, err := addNetwork(t.Context(), args, standalone(publish))
	if err != nil || result == nil || version != "0.3.1" {
		t.Fatalf("ADD of %s: result=%v version=%q err=%v", args.ContainerID, result, version, err)
	}
}

// The record is promoted when the write is acknowledged, and ADD saves the
// configuration again when it completes. The two must agree: a completed ADD
// whose publication was acknowledged leaves a confirmed record, not the
// intent it wrote before the write.
func TestACompletedADDLeavesAConfirmedRecord(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	args := sandboxArgs("completed")
	completeADD(t, api, args, annotationEntry("net-a", testPF, 0))
	if record := mustRecord(t, args); record.PublicationUnconfirmed {
		t.Fatalf("completed ADD left its record as intent: %+v", record)
	}
}

// kubelet can deliver an earlier sandbox's DEL after a later sandbox of the
// same Pod has completed its ADD for the same function, with the same GUID
// and index. The later record is confirmed, so the earlier DEL withdraws
// nothing; the current sandbox's member stays.
func TestAnEarlierSandboxsLateDELLeavesTheCompletedSuccessorsMember(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", testPF, 0)
	earlier, current := sandboxArgs("earlier-completed"), sandboxArgs("current-completed")
	completeADD(t, api, earlier, mine)
	completeADD(t, api, current, mine)
	earlier.Netns = ""
	if err := delNetwork(t.Context(), earlier, standalone(withdrawer(api))); err != nil {
		t.Fatalf("late DEL of the earlier sandbox: %v", err)
	}
	checkEntry(t, api, mine, 1)
	// The superseded sandbox is gone; its record has nothing left to settle
	// and must not stay behind for every later publication to read.
	if _, ok := recordedPublication(earlier); ok {
		t.Fatalf("the superseded sandbox %s kept its publication record", earlier.ContainerID)
	}
}

// A retried ADD records the member its previous confirmed publication left on
// the Pod, so an uncertain retry can still withdraw it. Once the retry is
// confirmed the Pod carries the new member and the previous one is
// superseded: the record forgets it, and a later sandbox's write of that
// same member is that sandbox's own to withdraw.
func TestAConfirmedRetryForgetsItsSupersededMember(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	a, b := sandboxArgs("retried"), sandboxArgs("later")
	a.Netns, b.Netns = "", ""
	superseded := annotationEntry("net-a", testPF, 1)
	replacement := annotationEntry("net-a", testPF, 0)
	publishFrom(t, api, a, superseded)
	api.getErr = errors.New("API unavailable")
	if err := delNetwork(t.Context(), a, standalone(withdrawer(api))); err == nil {
		t.Fatal("DEL during the API outage reported success")
	}
	api.getErr = nil
	if _, err := publishRecorded(t.Context(), api, a, publishingConf(t, a),
		func(string, string) (int, string, error) { return replacement.DeviceIndex, replacement.PfGUID, nil }); err != nil {
		t.Fatalf("retried publication of %s: %v", a.ContainerID, err)
	}
	record := mustRecord(t, a)
	if record.PublicationUnconfirmed || record.PriorPublishedPfGUID != "" {
		t.Fatalf("confirmed retry still claims its superseded member: %+v", record)
	}
	checkEntry(t, api, replacement, 1)

	landed := &unacknowledgedPatch{annotationClientFake: api, afterApply: func() {
		api.getErr = errors.New("API unavailable after the write landed")
	}}
	_, err := publishRecorded(t.Context(), landed, b, publishingConf(t, b),
		func(string, string) (int, string, error) { return superseded.DeviceIndex, superseded.PfGUID, nil })
	api.getErr = nil
	if !errors.Is(err, io.ErrUnexpectedEOF) || !mustRecord(t, b).PublicationUnconfirmed {
		t.Fatalf("later publication: err=%v, want an unknown outcome left as intent", err)
	}
	checkEntry(t, api, superseded, 1)
	if err := delNetwork(t.Context(), a, standalone(withdrawer(api))); err != nil {
		t.Fatalf("DEL of the retried sandbox %s: %v", a.ContainerID, err)
	}
	checkEntry(t, api, superseded, 1)
	if err := delNetwork(t.Context(), b, standalone(withdrawer(api))); err != nil {
		t.Fatalf("DEL of the later sandbox %s: %v", b.ContainerID, err)
	}
	if details := detailsOf(t, api); len(details) != 0 {
		t.Fatalf("the later sandbox's DEL left %+v", details)
	}
}
