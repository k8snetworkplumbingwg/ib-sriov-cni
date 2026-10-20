package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
)

// Throughout this file the cache and the API server deliberately hold
// different revisions, modeling a watch update that has not arrived yet. Each
// case is an operation whose cached answer is "nothing to do", which is the
// one answer a cache cannot be trusted to give.
func TestWithdrawalDoesNotTrustCachedAbsence(t *testing.T) {
	old := cachedPod()
	pods, _ := startNodeCache(t, old)
	api, args := newAnnotationTest(t)
	detail := annotationEntry("net-a", "0000:03:00.0", 0)
	api.pod.ResourceVersion = "2"
	api.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t,
		map[string]connectiondetails.Detail{detail.Key(): detail})
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}

	if err := removePodConnectionDetails(context.Background(), writer, args, detail.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	current, err := connectiondetails.Parse(api.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := current[detail.Key()]; exists {
		t.Fatalf("withdrawal reported success but the live record survives %s: live GETs=%d PATCHes=%d",
			detail.Key(), api.getCalls, api.patches)
	}
}

func TestPublicationDoesNotTrustCachedEquality(t *testing.T) {
	detail := annotationEntry("net-a", "0000:03:00.0", 0)
	old := cachedPod()
	old.Annotations[connectiondetails.Annotation] = encodeEntries(t,
		map[string]connectiondetails.Detail{detail.Key(): detail})
	pods, _ := startNodeCache(t, old)
	api, args := newAnnotationTest(t)
	api.pod.ResourceVersion = "2"
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}

	_, err := publishPodConnectionDetails(context.Background(), writer, args, detail.DeviceID,
		func(string, string) (int, string, error) { return detail.DeviceIndex, detail.PfGUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !connectiondetails.IsCurrent(api.pod.Annotations[connectiondetails.Annotation], detail) {
		t.Fatalf("publication reported success but the live record is absent %s: live GETs=%d PATCHes=%d",
			detail.Key(), api.getCalls, api.patches)
	}
}

// divergentClient serves reads from a cached copy and writes to a live copy,
// so a test can state exactly what each side holds without an informer.
type divergentClient struct {
	cached, live       *corev1.Pod
	liveReads, patches int
}

func (d *divergentClient) GetPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	if d.cached == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	}
	return d.cached.DeepCopy(), nil
}

func (d *divergentClient) GetPodAuthoritative(_ context.Context, _, _ string) (*corev1.Pod, error) {
	d.liveReads++
	if d.live == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	}
	return d.live.DeepCopy(), nil
}

func (d *divergentClient) PatchAnnotation(_ context.Context, _, _, value, rv string, _ bool) error {
	d.patches++
	if d.live == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	}
	if rv != d.live.ResourceVersion {
		return apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "vm-1", nil)
	}
	d.live.Annotations[connectiondetails.Annotation] = value
	d.live.ResourceVersion += "x"
	return nil
}

func divergentPod(t *testing.T, rv string, entries map[string]connectiondetails.Detail) *corev1.Pod {
	t.Helper()
	pod := cachedPod()
	pod.ResourceVersion = rv
	if entries != nil {
		pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, entries)
	}
	return pod
}

func TestConfirmedWriteKeepsASiblingAddedSinceTheCachedRevision(t *testing.T) {
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	sibling := annotationEntry("net-a", "0002:03:00.0", 1)
	_, args := newAnnotationTest(t)

	t.Run("publication", func(t *testing.T) {
		// Cache says this record is already there, so publication would stop.
		// The live object has lost it and gained a sibling in the meantime.
		client := &divergentClient{
			cached: divergentPod(t, "1", map[string]connectiondetails.Detail{mine.Key(): mine}),
			live:   divergentPod(t, "2", map[string]connectiondetails.Detail{sibling.Key(): sibling}),
		}
		if _, err := publishPodConnectionDetails(context.Background(), client, args, mine.DeviceID,
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
			t.Fatal(err)
		}
		left, err := connectiondetails.Parse(client.live.Annotations[connectiondetails.Annotation])
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 2 || left[mine.Key()] != mine || left[sibling.Key()] != sibling {
			t.Fatalf("live records = %+v, want both the republished one and the sibling", left)
		}
	})

	t.Run("withdrawal", func(t *testing.T) {
		// Cache says there is nothing of ours to remove; the live object has
		// ours plus a sibling that must survive.
		client := &divergentClient{
			cached: divergentPod(t, "1", map[string]connectiondetails.Detail{sibling.Key(): sibling}),
			live: divergentPod(t, "2", map[string]connectiondetails.Detail{
				mine.Key(): mine, sibling.Key(): sibling}),
		}
		if err := removePodConnectionDetails(context.Background(), client, args, mine.DeviceID, ""); err != nil {
			t.Fatal(err)
		}
		left, err := connectiondetails.Parse(client.live.Annotations[connectiondetails.Annotation])
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 1 || left[sibling.Key()] != sibling {
			t.Fatalf("live records = %+v, want only the sibling", left)
		}
	})
}

func TestAGenuineNoOpIsConfirmedAndWritesNothing(t *testing.T) {
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	_, args := newAnnotationTest(t)
	both := map[string]connectiondetails.Detail{mine.Key(): mine}

	client := &divergentClient{cached: divergentPod(t, "1", both), live: divergentPod(t, "1", both)}
	if _, err := publishPodConnectionDetails(context.Background(), client, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
		t.Fatal(err)
	}
	if client.patches != 0 || client.liveReads != 1 {
		t.Fatalf("republication wrote %d and read live %d times, want 0 writes and 1 confirming read",
			client.patches, client.liveReads)
	}

	client = &divergentClient{cached: divergentPod(t, "1", nil), live: divergentPod(t, "1", nil)}
	if err := removePodConnectionDetails(context.Background(), client, args, mine.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	if client.patches != 0 || client.liveReads != 1 {
		t.Fatalf("withdrawing nothing wrote %d and read live %d times, want 0 writes and 1 confirming read",
			client.patches, client.liveReads)
	}
}

func TestReplacementAndDisappearanceFollowEachCommandsContract(t *testing.T) {
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	_, args := newAnnotationTest(t)
	held := map[string]connectiondetails.Detail{mine.Key(): mine}

	t.Run("withdrawal leaves a replacement Pod alone", func(t *testing.T) {
		client := &divergentClient{cached: divergentPod(t, "1", nil), live: divergentPod(t, "2", held)}
		client.live.UID = "uid-2"
		if err := removePodConnectionDetails(context.Background(), client, args, mine.DeviceID, ""); err != nil {
			t.Fatal(err)
		}
		if client.patches != 0 {
			t.Fatal("withdrawal touched a Pod that had already been replaced")
		}
	})

	t.Run("withdrawal accepts a Pod that is gone", func(t *testing.T) {
		client := &divergentClient{cached: divergentPod(t, "1", nil), live: nil}
		if err := removePodConnectionDetails(context.Background(), client, args, mine.DeviceID, ""); err != nil {
			t.Fatalf("withdrawal from a deleted Pod = %v, want success", err)
		}
	})

	t.Run("publication fails when the Pod is gone", func(t *testing.T) {
		client := &divergentClient{cached: divergentPod(t, "1", held), live: nil}
		if _, err := publishPodConnectionDetails(context.Background(), client, args, mine.DeviceID,
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err == nil {
			t.Fatal("publication onto a deleted Pod reported success")
		}
	})
}

func TestOneRequestEscalatingLeavesOtherReadersOnTheCache(t *testing.T) {
	pods, _ := startNodeCache(t, cachedPod())
	api, _ := newAnnotationTest(t)
	shared := nodePodReader{pods: pods, api: api}
	escalated := &nodePodWriter{reader: shared}
	escalated.stale = true
	if _, err := escalated.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 1 {
		t.Fatalf("escalated reader made %d live reads, want 1", api.getCalls)
	}
	// Escalation is request-local: a second operation and a readiness waiter
	// sharing the same cache must not inherit it.
	other := &nodePodWriter{reader: shared}
	if _, err := other.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.GetPod(context.Background(), "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 1 {
		t.Fatalf("live reads = %d, want the other readers to stay on the cache", api.getCalls)
	}
}

// fencedAPI rejects a write whose resourceVersion is not the current one, the
// way the API server does, so a test can drive the writer's escalation.
type fencedAPI struct {
	pod            *corev1.Pod
	reads, patches int
}

func (f *fencedAPI) GetPod(context.Context, string, string) (*corev1.Pod, error) {
	f.reads++
	return f.pod.DeepCopy(), nil
}

func (f *fencedAPI) GetPodAuthoritative(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return f.GetPod(ctx, namespace, name)
}

func (f *fencedAPI) PatchAnnotation(_ context.Context, _, _, value, rv string, _ bool) error {
	f.patches++
	if rv != f.pod.ResourceVersion {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("stale"))
	}
	f.pod.Annotations[connectiondetails.Annotation] = value
	f.pod.ResourceVersion += "x"
	return nil
}

func TestARejectedWriteRemergesLiveAndKeepsTheSibling(t *testing.T) {
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	sibling := annotationEntry("net-a", "0002:03:00.0", 1)
	_, args := newAnnotationTest(t)
	// The cache is a revision behind and has never seen the sibling. The first
	// write is fenced on that revision and is rejected; the retry must read
	// live rather than rebuild the same losing patch from the same copy.
	pods, _ := startNodeCache(t, divergentPod(t, "1", nil))
	api := &fencedAPI{pod: divergentPod(t, "2", map[string]connectiondetails.Detail{sibling.Key(): sibling})}
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}

	if _, err := publishPodConnectionDetails(context.Background(), writer, args, mine.DeviceID,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
		t.Fatal(err)
	}
	left, err := connectiondetails.Parse(api.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[mine.Key()] != mine || left[sibling.Key()] != sibling {
		t.Fatalf("live records = %+v, want the published one and the sibling", left)
	}
	if !writer.stale {
		t.Fatal("a rejected precondition did not escalate the writer")
	}
	if api.patches < 2 {
		t.Fatalf("patches = %d, want a rejection and then a re-merge", api.patches)
	}
}

// The cached copy may still be a same-name predecessor of the Pod being set
// up. Its UID must not fail the publication: the authoritative copy is read, as
// the withdrawal already does for the same condition, and the live Pod is
// published to. A UID the authoritative copy also contradicts is a replacement
// and still fails, without a write.
func TestPublicationReadsLiveWhenTheCachedPodIsAPredecessor(t *testing.T) {
	cacheDirAt(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	predecessor := divergentPod(t, "1", nil)
	predecessor.UID = "uid-0"
	_, args := newAnnotationTest(t)
	t.Run("live successor is published to", func(t *testing.T) {
		client := &divergentClient{cached: predecessor, live: divergentPod(t, "2", nil)}
		published, err := publishPodConnectionDetails(t.Context(), client, args, mine.DeviceID,
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
		if err != nil {
			t.Fatalf("the cached predecessor failed the publication: %v", err)
		}
		if published.UID != "uid-1" || client.liveReads == 0 {
			t.Fatalf("published to UID %q after %d authoritative reads; want uid-1 after at least one", published.UID, client.liveReads)
		}
		if !connectiondetails.IsCurrent(client.live.Annotations[connectiondetails.Annotation], mine) {
			t.Fatal("the live Pod does not carry the publication")
		}
	})
	t.Run("live replacement still fails", func(t *testing.T) {
		replacement := divergentPod(t, "3", nil)
		replacement.UID = "uid-9"
		client := &divergentClient{cached: predecessor, live: replacement}
		_, err := publishPodConnectionDetails(t.Context(), client, args, mine.DeviceID,
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
		if err == nil || !strings.Contains(err.Error(), "UID mismatch") || client.patches != 0 {
			t.Fatalf("replacement: err=%v patches=%d; want a UID mismatch and no write", err, client.patches)
		}
	})
}
