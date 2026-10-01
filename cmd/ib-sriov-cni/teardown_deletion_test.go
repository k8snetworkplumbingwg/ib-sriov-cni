package main

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
)

// When the Pod itself is being deleted, its details are the controller's
// record of what to release, and the Pod object takes them with it when it
// goes. A teardown DEL therefore leaves them; only the rollback DEL of a Pod
// that lives on withdraws.
func TestDELLeavesTheDetailsOfAPodBeingDeleted(t *testing.T) {
	f, args := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	if _, err := publishPodConnectionDetails(t.Context(), f, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now())
	f.pod.DeletionTimestamp = &now
	patches := f.patches
	if err := removePodConnectionDetails(t.Context(), f, args, mine.DeviceID, ""); err != nil {
		t.Fatalf("DEL of a deleting Pod must succeed: %v", err)
	}
	if f.patches != patches {
		t.Fatalf("DEL edited a Pod that is being deleted: %d patches", f.patches-patches)
	}
	checkEntry(t, f, mine, 1)
}

// The cache can be a revision behind the deletion. The write is fenced on the
// cached revision, so it is rejected; the retry reads live, sees the Pod is
// being deleted, and stops rather than rebuilding the withdrawal.
func TestDELLearnsOfTheDeletionFromARejectedWrite(t *testing.T) {
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	_, args := newAnnotationTest(t)
	pods, _ := startNodeCache(t, divergentPod(t, "1", map[string]connectiondetails.Detail{mine.Key(): mine}))
	live := divergentPod(t, "2", map[string]connectiondetails.Detail{mine.Key(): mine})
	now := metav1.NewTime(time.Now())
	live.DeletionTimestamp = &now
	api := &fencedAPI{pod: live}
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}

	if err := removePodConnectionDetails(context.Background(), writer, args, mine.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	left, err := connectiondetails.Parse(api.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if left[mine.Key()] != mine {
		t.Fatalf("the deleting Pod lost its record: %+v", left)
	}
	if api.patches != 1 {
		t.Fatalf("patches = %d, want exactly the one the fence rejected", api.patches)
	}
}
