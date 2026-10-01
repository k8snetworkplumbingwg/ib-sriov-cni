package connectiondetails

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
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

// PodGetter reads one Pod. The standalone plugin satisfies it with an API read;
// a node daemon satisfies it from a shared informer cache, so the wait below is
// identical in both and the transport decides how many API calls it costs.
type PodGetter interface {
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
}

// WaitForReady blocks until ib-kubernetes reports key Ready on pod, the Pod is
// replaced, deleted or being deleted, or ctx expires. Absence of the key means
// the controller has not finished, so the wait continues; there is no failure
// value to observe. A Pod that carries a deletion timestamp will never become
// ready, and kubelet cannot tear its sandbox down until this ADD returns, so
// that ends the wait at once.
func WaitForReady(ctx context.Context, getter PodGetter, namespace, name, uid, key string) (retErr error) {
	reads := 0
	defer func() {
		observability.Readiness(ctx, readinessOutcome(reads, retErr))
	}()
	misses := 0
	for {
		reads++
		pod, err := getter.GetPod(ctx, namespace, name)
		switch {
		case apierrors.IsNotFound(err):
			misses++
			if misses >= readinessMissLimit {
				return fmt.Errorf("pod %s/%s is gone while waiting for %q to become ready",
					namespace, name, key)
			}
		case err != nil:
			return err
		default:
			misses = 0
			if uid != "" && string(pod.UID) != uid {
				return fmt.Errorf("pod %s/%s was replaced while waiting for %q: expected UID %s, found %s",
					namespace, name, key, uid, pod.UID)
			}
			if pod.DeletionTimestamp != nil {
				return fmt.Errorf("pod %s/%s is being deleted while waiting for %q to become ready", namespace, name, key)
			}
			ready, parseErr := IsReady(pod.Annotations[StatusAnnotation], key)
			if parseErr != nil {
				return parseErr
			}
			if ready {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %q to become ready: %w", key, ctx.Err())
		case <-time.After(readinessPollInterval):
		}
	}
}

func readinessOutcome(reads int, err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return observability.ResultTimeout
	case errors.Is(err, context.Canceled):
		return observability.ResultCanceled
	case err != nil:
		return observability.ResultError
	case reads == 1:
		return "first_read_ready"
	default:
		return "waited_ready"
	}
}
