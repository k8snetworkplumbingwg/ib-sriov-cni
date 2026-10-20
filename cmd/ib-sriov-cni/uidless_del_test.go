package main

import (
	"testing"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
)

// A delayed DEL without K8S_POD_UID must not remove a same-name replacement
// Pod's allocation after the device plugin has reassigned the same BDF.
func TestUIDLessDelayedDELDoesNotWithdrawReplacement(t *testing.T) {
	f, args := newAnnotationTest(t)
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1"
	mine := annotationEntry("net-a", "0000:03:00.0", 0)

	if _, err := publishPodConnectionDetails(t.Context(), f, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
		t.Fatal(err)
	}

	replacement := mine
	replacement.PfGUID = "90:e3:17:03:00:fb:91:99"
	f.pod = f.pod.DeepCopy()
	f.pod.UID = "uid-2"
	f.pod.ResourceVersion = "100"
	f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t,
		map[string]connectiondetails.Detail{replacement.Key(): replacement})

	if err := removePodConnectionDetails(t.Context(), f, args, mine.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	details, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if got, present := details[replacement.Key()]; !present || got != replacement {
		t.Fatalf("old UID-less DEL removed replacement ownership: details=%+v", details)
	}
}
