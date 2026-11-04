package connectiondetails

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNormalizeAPIServerHost(t *testing.T) {
	cases := map[string]string{
		"https://[10.96.0.1]:443":  "https://10.96.0.1:443",
		"https://10.96.0.1:443":    "https://10.96.0.1:443",
		"https://[fd00::1]:6443":   "https://[fd00::1]:6443",
		"https://api.example:6443": "https://api.example:6443",
	}
	for in, want := range cases {
		if got := NormalizeAPIServerHost(in); got != want {
			t.Fatalf("NormalizeAPIServerHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// A pod with no annotations map must still get the annotation: the JSON Patch
// adds the whole map instead of a key under a missing parent.
func TestPatchAnnotationCreatesAbsentAnnotationsMap(t *testing.T) {
	// A real pod always carries a resourceVersion (ObjectMeta omits an empty
	// one, which would make the patch's `test` op fail on a missing path).
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "p", ResourceVersion: "1"}} // Annotations == nil
	cs := fake.NewSimpleClientset(pod)
	c := &PodClient{clientset: cs}

	snapshot, err := c.GetPod(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	if snapshot.Annotations != nil {
		t.Fatalf("expected absent annotations map, got %#v", snapshot.Annotations)
	}
	value, err := MergeAnnotation("", testDetail())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PatchAnnotation(context.Background(), "ns", "p", value, snapshot.ResourceVersion, snapshot.Annotations != nil); err != nil {
		t.Fatalf("PatchAnnotation on absent-map pod: %v", err)
	}
	got, err := cs.CoreV1().Pods("ns").Get(context.TODO(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[Annotation] != value {
		t.Fatalf("annotation not set on absent-map pod: %#v", got.Annotations)
	}
}

// Steady state: a pod that already has annotations takes the escaped-key
// patch path, and a merged value preserves the existing entry.
func TestPatchAnnotationMergesIntoExistingMap(t *testing.T) {
	first := testDetail()
	value, err := MergeAnnotation("", first)
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "p", ResourceVersion: "1",
		Annotations: map[string]string{Annotation: value, "unrelated": "preserved"}}}
	cs := fake.NewSimpleClientset(pod)
	c := &PodClient{clientset: cs}

	snapshot, err := c.GetPod(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("GetPod: %v", err)
	}
	second := first
	second.DeviceID, second.DeviceIndex = "0002:03:00.0", 1
	merged, err := MergeAnnotation(snapshot.Annotations[Annotation], second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PatchAnnotation(context.Background(), "ns", "p", merged, snapshot.ResourceVersion, snapshot.Annotations != nil); err != nil {
		t.Fatalf("PatchAnnotation on existing-map pod: %v", err)
	}
	got, err := cs.CoreV1().Pods("ns").Get(context.TODO(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	details, err := Parse(got.Annotations[Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if details[first.Key()] != first || details[second.Key()] != second {
		t.Fatalf("merged annotation lost an entry: %#v", details)
	}
	if got.Annotations["unrelated"] != "preserved" {
		t.Fatal("patch lost unrelated annotation")
	}
}

func TestGetPodReturnsCoherentSnapshot(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "p", UID: types.UID("pod-uid"), ResourceVersion: "42",
		Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": "ib-pf", Annotation: "{}"},
	}}
	c := &PodClient{clientset: fake.NewSimpleClientset(pod)}
	got, err := c.GetPod(context.Background(), "ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != pod.UID || got.ResourceVersion != "42" ||
		got.Annotations["k8s.v1.cni.cncf.io/networks"] != "ib-pf" || got.Annotations[Annotation] != "{}" {
		t.Fatalf("incomplete pod snapshot: %#v", got.ObjectMeta)
	}
	if _, err := c.GetPod(context.Background(), "ns", "missing"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected wrapped NotFound, got %v", err)
	}
}

func TestPatchAnnotationRejectsStaleResourceVersion(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "p", ResourceVersion: "2", Annotations: map[string]string{Annotation: "{}"},
	}}
	c := &PodClient{clientset: fake.NewSimpleClientset(pod)}
	if err := c.PatchAnnotation(context.Background(), "ns", "p", "{}", "1", true); err == nil {
		t.Fatal("stale resourceVersion patch succeeded")
	}
}
