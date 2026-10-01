package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
)

func TestWithdrawalConfirmsCachedUIDMismatch(t *testing.T) {
	old := cachedPod()
	pods, _ := startNodeCache(t, old)
	api, args := newAnnotationTest(t)
	detail := annotationEntry("net-a", "0000:03:00.0", 0)
	api.pod.UID = "uid-2"
	api.pod.ResourceVersion = "2"
	api.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t,
		map[string]connectiondetails.Detail{detail.Key(): detail})
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1;K8S_POD_UID=uid-2"
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}

	if err := removePodConnectionDetails(context.Background(), writer, args, detail.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	if connectiondetails.IsCurrent(api.pod.Annotations[connectiondetails.Annotation], detail) {
		t.Fatalf("DEL for live UID uid-2 returned success against cached UID uid-1; live record remains: live GETs=%d PATCHes=%d",
			api.getCalls, api.patches)
	}
	if api.getCalls != 1 || api.patches != 1 {
		t.Fatalf("live GETs=%d PATCHes=%d, want one confirmation and one withdrawal", api.getCalls, api.patches)
	}
}

func TestWithdrawalUIDConfirmationPreservesLiveOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		getErr error
	}{
		{name: "replacement"},
		{name: "deleted", getErr: apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")},
		{name: "forbidden", getErr: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("denied"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached := cachedPod()
			cached.UID = "uid-2"
			pods, _ := startNodeCache(t, cached)
			api, args := newAnnotationTest(t)
			api.pod.UID = "uid-2"
			detail := annotationEntry("net-a", "0000:03:00.0", 0)
			original := encodeEntries(t, map[string]connectiondetails.Detail{detail.Key(): detail})
			api.pod.Annotations[connectiondetails.Annotation] = original
			api.getErr = tc.getErr
			writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}

			err := removePodConnectionDetails(context.Background(), writer, args, detail.DeviceID, "")
			if apierrors.IsForbidden(tc.getErr) {
				if !apierrors.IsForbidden(err) {
					t.Fatalf("withdrawal=%v, want authoritative Forbidden", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if api.getCalls != 1 || api.patches != 0 {
				t.Fatalf("live GETs=%d PATCHes=%d, want one confirmation and no writes", api.getCalls, api.patches)
			}
			if api.pod.Annotations[connectiondetails.Annotation] != original {
				t.Fatal("withdrawal changed another Pod UID's details")
			}
		})
	}
}

func TestRememberedNotFoundEndsReadinessWait(t *testing.T) {
	pods, _ := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	api.getErr = apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	reader := newRecheckingReader(nodePodReader{pods: pods, api: api}, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	err := connectiondetails.WaitForReady(ctx, reader, "ns1", "vm-1", "uid-1", "ns1/net-a/0000:03:00.0")
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "is gone") || ctx.Err() != nil {
		t.Fatalf("wait result=%v context=%v; want the five-miss terminal error before timeout", err, ctx.Err())
	}
	if api.getCalls != 1 {
		t.Fatalf("live GETs=%d, want one reused negative answer", api.getCalls)
	}
	t.Logf("five-miss wait ended in %s with %d live GET: %v", elapsed, api.getCalls, err)
}

func TestCacheArrivalOverridesRememberedNotFound(t *testing.T) {
	pods, kube := startNodeCache(t)
	api, _ := newAnnotationTest(t)
	api.getErr = apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "vm-1")
	reader := newRecheckingReader(nodePodReader{pods: pods, api: api}, time.Hour)
	if _, err := reader.GetPod(context.Background(), "ns1", "vm-1"); !apierrors.IsNotFound(err) {
		t.Fatalf("initial read=%v, want remembered NotFound", err)
	}
	ready := cachedPod()
	ready.Annotations[connectiondetails.StatusAnnotation] = `{"ns1/net-a/0000:03:00.0":{"Status":"Ready"}}`
	if _, err := kube.CoreV1().Pods("ns1").Create(context.Background(), ready, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := pods.Get("ns1", "vm-1"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("created Pod never reached cache")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := connectiondetails.WaitForReady(ctx, reader, "ns1", "vm-1", "uid-1", "ns1/net-a/0000:03:00.0"); err != nil {
		t.Fatal(err)
	}
	if api.getCalls != 1 {
		t.Fatalf("live GETs=%d, want cache arrival accepted without another API request", api.getCalls)
	}
}
