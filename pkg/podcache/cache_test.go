package podcache

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: name, UID: types.UID(name), ResourceVersion: "1"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func startCache(t *testing.T, objects ...runtime.Object) (*Cache, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset(objects...)
	watchStarted := make(chan struct{})
	var once sync.Once
	client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		once.Do(func() { close(watchStarted) })
		return false, nil, nil
	})
	c, err := New(client, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("cache Run = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("cache did not shut down")
		}
	})
	if !c.WaitForSync(testContext(t)) {
		t.Fatal("cache did not sync")
	}
	select {
	case <-watchStarted:
	case <-testContext(t).Done():
		t.Fatal("watch did not start")
	}
	return c, client
}

func TestNewRejectsMissingConfiguration(t *testing.T) {
	if _, err := New(fake.NewClientset(), ""); err == nil {
		t.Fatal("empty node name accepted")
	}
	if _, err := New(fake.NewClientset(), "  "); err == nil {
		t.Fatal("blank node name accepted")
	}
	if _, err := New(nil, "node-a"); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestGetRequiresActiveInitialSync(t *testing.T) {
	c, err := New(fake.NewClientset(testPod("a")), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("tenant", "a"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Get before Run = %v, want ErrNotReady", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.WaitForSync(ctx) {
		t.Fatal("unsynced cache reported ready")
	}
}

func TestOneNodeFilteredListWatchAndCacheOnlyReads(t *testing.T) {
	other := testPod("other")
	other.Spec.NodeName = "node-b"
	c, client := startCache(t, testPod("a"), other)
	for range 100 {
		if _, err := c.Get("tenant", "a"); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"missing", "other"} {
			if _, err := c.Get("tenant", name); !apierrors.IsNotFound(err) {
				t.Fatalf("Get(%s) = %v, want NotFound", name, err)
			}
		}
	}
	actions := client.Actions()
	if len(actions) != 2 {
		t.Fatalf("API actions = %v, want one LIST and one WATCH", actions)
	}
	for _, action := range actions {
		var selector string
		switch action := action.(type) {
		case k8stesting.ListAction:
			selector = action.GetListRestrictions().Fields.String()
		case k8stesting.WatchAction:
			selector = action.GetWatchRestrictions().Fields.String()
		default:
			t.Fatalf("unexpected API action: %v", action)
		}
		if selector != "spec.nodeName=node-a" {
			t.Fatalf("field selector = %q", selector)
		}
		if action.GetNamespace() != metav1.NamespaceAll {
			t.Fatalf("watch restricted to namespace %q", action.GetNamespace())
		}
	}
}

func TestCacheRetainsOnlyNetworkObservationFields(t *testing.T) {
	pod := testPod("a")
	deleted := metav1.Now()
	pod.DeletionTimestamp = &deleted
	pod.Annotations = map[string]string{
		"k8s.v1.cni.cncf.io/networks":           "ib1,ib2",
		"v1.ib-sriov-cni.io/connection-details": "details",
		"v1.ib-sriov-cni.io/connection-status":  "status",
		"unrelated":                             "private",
	}
	pod.Labels = map[string]string{"private": "value"}
	pod.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "controller"}}
	pod.Spec.Containers = []corev1.Container{{Name: "vm", Env: []corev1.EnvVar{{Name: "SECRET", Value: "private"}}}}
	pod.Status.Phase = corev1.PodRunning
	c, _ := startCache(t, pod)
	got, err := c.Get("tenant", "a")
	if err != nil {
		t.Fatal(err)
	}
	want := testPod("a")
	want.DeletionTimestamp = &deleted
	want.Annotations = map[string]string{
		"k8s.v1.cni.cncf.io/networks":           "ib1,ib2",
		"v1.ib-sriov-cni.io/connection-details": "details",
		"v1.ib-sriov-cni.io/connection-status":  "status",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cache contains unexpected Pod fields: %#v", got)
	}
	got.Annotations["v1.ib-sriov-cni.io/connection-details"] = "mutated"
	got.DeletionTimestamp.Time = time.Time{}
	again, err := c.Get("tenant", "a")
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("caller mutation reached cache: %#v, %v", again, err)
	}
	stored, exists, err := c.informer.GetStore().GetByKey("tenant/a")
	if err != nil || !exists || !reflect.DeepEqual(stored, want) {
		t.Fatalf("informer retained full Pod: %#v, %v", stored, err)
	}
}

func TestCacheReportsWhetherAnAnnotationMapExists(t *testing.T) {
	untracked := testPod("untracked")
	untracked.Annotations = map[string]string{"unrelated": "private"}
	bare := testPod("bare")
	c, _ := startCache(t, untracked, bare)
	// A writer decides from this copy whether its patch must create the
	// annotation map, and creating it replaces every annotation on the Pod.
	got, err := c.Get("tenant", "untracked")
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations == nil {
		t.Fatal("Pod with only untracked annotations reported no annotation map")
	}
	if len(got.Annotations) != 0 {
		t.Fatalf("untracked annotations retained: %v", got.Annotations)
	}
	if got, err = c.Get("tenant", "bare"); err != nil {
		t.Fatal(err)
	}
	if got.Annotations != nil {
		t.Fatalf("Pod without annotations reported a map: %v", got.Annotations)
	}
}

func TestInitialSyncDeadlineAndShutdown(t *testing.T) {
	client := fake.NewClientset()
	listStarted := make(chan struct{})
	release := make(chan struct{})
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		close(listStarted)
		<-release
		return true, &corev1.PodList{}, nil
	})
	c, err := New(client, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	<-listStarted
	deadline, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if c.WaitForSync(deadline) {
		t.Fatal("blocked initial LIST reported synced")
	}
	if _, err := c.Get("tenant", "missing"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Get during initial LIST = %v", err)
	}
	cancel()
	close(release) // Fake reactors do not implement HTTP context cancellation.
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testContext(t).Done():
		t.Fatal("Run did not join informer")
	}
}
