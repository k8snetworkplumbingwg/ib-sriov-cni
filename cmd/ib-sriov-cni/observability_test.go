package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

func TestPublicationObservesMergeRetriesWithoutChangingWrites(t *testing.T) {
	api, args := newAnnotationTest(t)
	api.patchHook = func() error {
		if api.patches == 1 {
			return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("stale"))
		}
		return nil
	}
	ctx, request := observability.Start(t.Context(), "daemon", "ADD", nil)
	_, err := publishPodConnectionDetails(ctx, api, args, "0000:03:00.0", func(string, string) (int, string, error) {
		return 0, "90:e3:17:03:00:fb:91:8e", nil
	})
	request.Finish(err)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := request.Snapshot()
	if api.patches != 2 || snapshot.PatchAttempts != 2 || snapshot.PatchConflicts != 1 {
		t.Fatalf("writes=%d observation=%+v", api.patches, snapshot)
	}
	if snapshot.Identity.Pod != "vm-1" || snapshot.Identity.UID != "uid-1" {
		t.Fatalf("missing resolved identity: %+v", snapshot.Identity)
	}
	if len(snapshot.Phases) < 2 || snapshot.Phases[len(snapshot.Phases)-1].Name != "annotation_merge" {
		t.Fatalf("missing merge timing: %+v", snapshot.Phases)
	}
}

func TestReadReasonsFollowOnlyActualFallbackReads(t *testing.T) {
	pods, _ := startNodeCache(t)
	kube := fake.NewClientset(cachedPod())
	reader := nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(kube)}
	metrics := observability.NewMetrics()
	ctx, request := observability.Start(t.Context(), "daemon", "ADD", metrics)
	if _, err := reader.GetPod(ctx, "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	writer := &nodePodWriter{reader: reader, stale: true}
	if _, err := writer.GetPod(ctx, "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.GetPodAuthoritative(ctx, "ns1", "vm-1"); err != nil {
		t.Fatal(err)
	}
	request.Finish(nil)
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	for _, reason := range []string{"cache_miss", "stale_write", "current_confirmation"} {
		want := `ib_sriov_cni_live_pod_gets_total{reason="` + reason + `",result="success"} 1`
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s: %s", want, w.Body.String())
		}
	}
	if len(kube.Actions()) != 3 {
		t.Fatalf("unexpected actual reads: %v", kube.Actions())
	}
}

func TestInspectionLockCancellationIsObserved(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ctx, request := observability.Start(ctx, "daemon", "ADD", nil)
	_, err := acquireObservedLock(ctx, t.TempDir(), "test.lock", "device_lock")
	request.Finish(err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	snapshot := request.Snapshot()
	if len(snapshot.Phases) != 1 || snapshot.Phases[0].Name != "device_lock" || snapshot.Phases[0].Result != "canceled" {
		t.Fatalf("lost cancellation phase: %+v", snapshot)
	}
}

func TestRememberedAbsencesDoNotCountAsLiveGETs(t *testing.T) {
	pods, _ := startNodeCache(t)
	kube := fake.NewClientset()
	reader := newRecheckingReader(nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(kube)}, time.Hour)
	metrics := observability.NewMetrics()
	ctx, request := observability.Start(t.Context(), "daemon", "ADD", metrics)
	for range 20 {
		if _, err := reader.GetPod(ctx, "ns1", "absent"); !apierrors.IsNotFound(err) {
			t.Fatalf("error=%v", err)
		}
	}
	if request.Snapshot().LiveGETs != 1 {
		t.Fatal("remembered absence counted as another live read")
	}
	reader.next = time.Now().Add(-time.Second)
	if _, err := reader.GetPod(ctx, "ns1", "absent"); !apierrors.IsNotFound(err) {
		t.Fatalf("error=%v", err)
	}
	request.Finish(nil)
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	for _, reason := range []string{"cache_miss", "periodic_recheck"} {
		want := `ib_sriov_cni_live_pod_gets_total{reason="` + reason + `",result="error"} 1`
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s: %s", want, w.Body.String())
		}
	}
	if len(kube.Actions()) != 2 || request.Snapshot().LiveGETs != 2 {
		t.Fatalf("API actions=%v, observation=%+v", kube.Actions(), request.Snapshot())
	}
}

func TestCallbackPanicIsNotLoggedAsSuccess(t *testing.T) {
	t.Cleanup(func() { klog.SetOutput(os.Stderr) })
	path := filepath.Join(pluginLogRootAt(t), "callback.log")
	conf, err := json.Marshal(map[string]string{
		"cniVersion": "1.0.0", "name": "net-a", "type": "ib-sriov", "logFile": "callback.log",
	})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != "callback panicked" {
				t.Fatalf("panic changed or swallowed: %v", recovered)
			}
		}()
		_ = executeCNICommand(&skel.CmdArgs{StdinData: conf}, "ADD", func(context.Context) error {
			panic("callback panicked")
		})
	}()
	klog.Flush()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "CNI request completed") != 1 || !strings.Contains(string(data), `result="error"`) {
		t.Fatalf("panic completion was not an error: %s", data)
	}
}
