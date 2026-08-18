package connectiondetails

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		if got := normalizeAPIServerHost(in); got != want {
			t.Fatalf("normalizeAPIServerHost(%q) = %q, want %q", in, got, want)
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

	val, rv, present, err := c.GetAnnotation(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("GetAnnotation: %v", err)
	}
	if present || val != "" {
		t.Fatalf("expected absent annotations map, got present=%v val=%q", present, val)
	}
	if err := c.PatchAnnotation(context.Background(), "ns", "p", `{"0":"aa"}`, rv, present); err != nil {
		t.Fatalf("PatchAnnotation on absent-map pod: %v", err)
	}
	got, err := cs.CoreV1().Pods("ns").Get(context.TODO(), "p", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[Annotation] != `{"0":"aa"}` {
		t.Fatalf("annotation not set on absent-map pod: %#v", got.Annotations)
	}
}

// Steady state: a pod that already has annotations takes the escaped-key
// patch path, and a merged value preserves the existing entry.
func TestPatchAnnotationMergesIntoExistingMap(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns", Name: "p", ResourceVersion: "1",
		Annotations: map[string]string{Annotation: `{"0":"aa"}`}}}
	cs := fake.NewSimpleClientset(pod)
	c := &PodClient{clientset: cs}

	val, rv, present, err := c.GetAnnotation(context.Background(), "ns", "p")
	if err != nil || !present {
		t.Fatalf("GetAnnotation: err=%v present=%v", err, present)
	}
	merged, err := MergeAnnotation(val, "2", "bb")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PatchAnnotation(context.Background(), "ns", "p", merged, rv, present); err != nil {
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
	if details["0"] != "aa" || details["2"] != "bb" {
		t.Fatalf("merged annotation lost an entry: %#v", details)
	}
}
