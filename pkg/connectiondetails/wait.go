package connectiondetails

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	// readinessPollInterval paces the wait between reads of the Pod. It follows
	// the ovn-kubernetes DPU-host handshake, which polls its local cache at the
	// same cadence rather than opening a watch per request.
	readinessPollInterval = 200 * time.Millisecond

	// readinessMissLimit bounds how long a Pod may be absent from the reader
	// before the wait gives up. A newly scheduled Pod can lag a cache briefly;
	// a Pod that stays missing has been deleted and will never become ready.
	readinessMissLimit = 5
)

// PodGetter reads one Pod. The plugin satisfies it with an API read; the wait
// below does not depend on where the Pod comes from.
type PodGetter interface {
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
}

// WaitForReady blocks until ib-kubernetes reports key Ready on pod, the Pod is
// replaced, deleted or being deleted, or ctx expires. Absence of the key means
// the controller has not finished, so the wait continues; there is no failure
// value to observe. A Pod that carries a deletion timestamp will never become
// ready, and kubelet cannot tear its sandbox down until this ADD returns, so
// that ends the wait at once. A read that fails for a reason that may clear,
// such as a restarting API server, is retried within the budget; a refused
// read ends the wait, since no later read would be answered differently.
func WaitForReady(ctx context.Context, getter PodGetter, namespace, name, uid, key string) error {
	misses := 0
	var lastErr error
	for {
		pod, err := getter.GetPod(ctx, namespace, name)
		switch {
		case err != nil && ctx.Err() != nil:
			return timeoutError(key, ctx.Err(), err)
		case apierrors.IsNotFound(err):
			misses++
			if misses >= readinessMissLimit {
				return fmt.Errorf("pod %s/%s is gone while waiting for %q to become ready",
					namespace, name, key)
			}
		case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
			return err
		case err != nil:
			lastErr = err
		default:
			misses = 0
			lastErr = nil
			ready, readyErr := podReady(pod, namespace, name, uid, key)
			if readyErr != nil {
				return readyErr
			}
			if ready {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return timeoutError(key, ctx.Err(), lastErr)
		case <-time.After(readinessPollInterval):
		}
	}
}

// podReady reports whether pod carries Ready for key, or why the wait cannot
// end well: the Pod is a replacement, it is being deleted, or its status
// cannot be read.
func podReady(pod *corev1.Pod, namespace, name, uid, key string) (bool, error) {
	if uid != "" && string(pod.UID) != uid {
		return false, fmt.Errorf("pod %s/%s was replaced while waiting for %q: expected UID %s, found %s",
			namespace, name, key, uid, pod.UID)
	}
	if pod.DeletionTimestamp != nil {
		return false, fmt.Errorf("pod %s/%s is being deleted while waiting for %q to become ready", namespace, name, key)
	}
	return IsReady(pod.Annotations[StatusAnnotation], key)
}

// timeoutError reports an expired budget, naming the last failed read when
// the wait ended on one.
func timeoutError(key string, cause, lastRead error) error {
	if lastRead != nil {
		return fmt.Errorf("timed out waiting for %q to become ready: %w (last read: %v)", key, cause, lastRead)
	}
	return fmt.Errorf("timed out waiting for %q to become ready: %w", key, cause)
}
