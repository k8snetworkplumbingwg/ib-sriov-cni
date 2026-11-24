package main

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
