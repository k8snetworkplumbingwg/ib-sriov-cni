package connectiondetails

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const waitKey = "tenant/ib-pf/0000:03:00.0"

type getterFunc func(ctx context.Context, namespace, name string) (*corev1.Pod, error)

func (f getterFunc) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return f(ctx, namespace, name)
}

func podWithStatus(uid, status string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "tenant", Name: "vm-1", UID: types.UID(uid),
	}}
	if status != "" {
		pod.Annotations = map[string]string{StatusAnnotation: status}
	}
	return pod
}

func TestWaitForReadyReturnsWhenAlreadyReady(t *testing.T) {
	reads := 0
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		reads++
		return podWithStatus("uid-1", readyStatus), nil
	})
	if err := WaitForReady(t.Context(), getter, "tenant", "vm-1", "uid-1", waitKey); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads=%d; an already-ready attachment must not be polled again", reads)
	}
}

func TestWaitForReadyReturnsWhenReadyArrivesLater(t *testing.T) {
	reads := 0
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		reads++
		if reads < 3 {
			// Published, controller has not finished: the key is simply absent.
			return podWithStatus("uid-1", "{}"), nil
		}
		return podWithStatus("uid-1", readyStatus), nil
	})
	if err := WaitForReady(t.Context(), getter, "tenant", "vm-1", "uid-1", waitKey); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reads != 3 {
		t.Fatalf("reads=%d, want 3", reads)
	}
}

func TestWaitForReadyTimesOutWhenNeverReady(t *testing.T) {
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		return podWithStatus("uid-1", "{}"), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := WaitForReady(ctx, getter, "tenant", "vm-1", "uid-1", waitKey)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting") {
		t.Fatalf("err=%v; a never-ready attachment must fail the ADD, not succeed", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v; the deadline cause must be preserved", err)
	}
}

func TestWaitForReadyKeepsWaitingThroughUnrecognisedStatus(t *testing.T) {
	// Only Ready is defined. Anything else is an unknown state, not a failure
	// signal, so the wait must not turn it into an early success or an error.
	reads := 0
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		reads++
		return podWithStatus("uid-1", `{"tenant/ib-pf/0000:03:00.0":{"Status":"Pending"}}`), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := WaitForReady(ctx, getter, "tenant", "vm-1", "uid-1", waitKey); err == nil {
		t.Fatal("an unrecognized status must not satisfy the wait")
	}
	if reads < 2 {
		t.Fatalf("reads=%d; the wait must keep polling through an unknown value", reads)
	}
}

func TestWaitForReadyFailsOnPodReplacement(t *testing.T) {
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		return podWithStatus("replacement", readyStatus), nil
	})
	err := WaitForReady(t.Context(), getter, "tenant", "vm-1", "uid-1", waitKey)
	if err == nil || !strings.Contains(err.Error(), "was replaced") {
		t.Fatalf("err=%v; a ready belonging to a replacement Pod must be refused", err)
	}
}

func TestWaitForReadyFailsWhenPodStaysMissing(t *testing.T) {
	reads := 0
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		reads++
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	})
	err := WaitForReady(t.Context(), getter, "tenant", "vm-1", "uid-1", waitKey)
	if err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("err=%v; a deleted Pod must fail fast rather than wait out the deadline", err)
	}
	if reads != readinessMissLimit {
		t.Fatalf("reads=%d, want %d; brief absence must be tolerated before giving up", reads, readinessMissLimit)
	}
}

func TestWaitForReadyToleratesTransientAbsence(t *testing.T) {
	reads := 0
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		reads++
		if reads <= readinessMissLimit-1 {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
		}
		return podWithStatus("uid-1", readyStatus), nil
	})
	if err := WaitForReady(t.Context(), getter, "tenant", "vm-1", "uid-1", waitKey); err != nil {
		t.Fatalf("a Pod that appears after brief lag must still be waited for: %v", err)
	}
}

func TestWaitForReadyPropagatesReadAndParseFailures(t *testing.T) {
	wantErr := errors.New("api is down")
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		return nil, wantErr
	})
	if err := WaitForReady(t.Context(), getter, "tenant", "vm-1", "uid-1", waitKey); !errors.Is(err, wantErr) {
		t.Fatalf("err=%v; a real read failure must not be mistaken for absence", err)
	}

	malformed := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		return podWithStatus("uid-1", "not-json"), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := WaitForReady(ctx, malformed, "tenant", "vm-1", "uid-1", waitKey)
	if err == nil || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v; an unreadable status must fail rather than be waited out", err)
	}
}

// A Pod that is being deleted will never become ready, and kubelet cannot
// tear its sandbox down until this ADD returns. The wait ends at once.
func TestWaitForReadyStopsWhenThePodIsBeingDeleted(t *testing.T) {
	reads := 0
	getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
		reads++
		pod := podWithStatus("uid-1", "")
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
		return pod, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	started := time.Now()
	err := WaitForReady(ctx, getter, "tenant", "vm-1", "uid-1", waitKey)
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("wait on a deleting Pod returned %v, want a being-deleted error", err)
	}
	if time.Since(started) > time.Second || reads != 1 {
		t.Fatalf("wait took %s and %d reads; it must stop on the first read", time.Since(started), reads)
	}
}
