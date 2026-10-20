package main

import (
	"testing"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// Older Multus can omit K8S_POD_UID. Publication already
// discovers the original UID, so the readiness phase must not discard it.
func TestReadyDoesNotAcknowledgeReplacementAfterPublication(t *testing.T) {
	f, args := newAnnotationTest(t)
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1"
	want := annotationEntry("net-a", "0000:03:00.0", 0)
	published, err := publishPodConnectionDetails(t.Context(), f, args, want.DeviceID,
		func(string, string) (int, string, error) { return 0, want.PfGUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	if f.pod.UID != "uid-1" || f.patches != 1 || published.UID != "uid-1" {
		t.Fatalf("original Pod was not published, or its UID was not carried: %+v", published)
	}
	key := published.Key
	// Replacement exists by the readiness read, under the same name and NAD.
	f.pod = f.pod.DeepCopy()
	f.pod.UID = "replacement-uid"
	f.pod.ResourceVersion = "100"
	f.pod.Annotations[connectiondetails.StatusAnnotation] =
		`{"` + key + `":{"Status":"Ready","pfGuid":"` + want.PfGUID + `"}}`
	conf := &localtypes.NetConf{}
	conf.ReadinessTimeoutSeconds = 1
	if err := awaitConnectionReady(t.Context(), f, args, conf, published); err == nil {
		t.Fatal("ADD accepted replacement Pod Ready after publishing only to uid-1")
	}
}
