package observability

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	require.Equal(t, 200, w.Code)
	return w.Body.String()
}

func TestRequestFinishOnceAndPhases(t *testing.T) {
	m := NewMetrics()
	ctx, request := Start(context.Background(), "daemon", "ADD", m)
	require.Contains(t, scrape(t, m), `ib_sriov_cni_requests_inflight{command="ADD"} 1`)
	SetIdentity(ctx, Identity{Namespace: "private-tenant", Pod: "launcher", Interface: "ib1"})
	SetIdentity(ctx, Identity{UID: "pod-uid", DeviceID: "0000:03:00.0"})
	phase := Phase(ctx, "device_inspection")
	detail := Detail(ctx, "vfio_restore")
	detail(nil)
	detail(errors.New("ignored second completion"))
	phase(nil)
	phase(nil)
	Readiness(ctx, "first_read_ready")
	LivePodGet(ctx, "cache_miss", nil)
	Patch(ctx, errors.New("conflict"), true)
	Patch(ctx, nil, false)
	DiscoveryPoll(ctx)
	request.Finish(nil)
	request.Finish(errors.New("ignored second completion"))
	snapshot := request.Snapshot()
	require.Equal(t, "success", snapshot.Result)
	require.Equal(t, "pod-uid", snapshot.Identity.UID)
	require.Equal(t, "private-tenant", snapshot.Identity.Namespace)
	require.Len(t, snapshot.Phases, 1)
	require.Len(t, snapshot.Details, 1)
	require.Equal(t, "first_read_ready", snapshot.Readiness)
	require.Equal(t, 1, snapshot.LiveGETs)
	require.Equal(t, 2, snapshot.PatchAttempts)
	require.Equal(t, 1, snapshot.PatchConflicts)
	require.Equal(t, 1, snapshot.DiscoveryPolls)
	require.Positive(t, snapshot.Duration)
	text := scrape(t, m)
	require.Contains(t, text, `ib_sriov_cni_requests_inflight{command="ADD"} 0`)
	require.Contains(t, text, `ib_sriov_cni_request_duration_seconds_count{command="ADD",result="success"} 1`)
	require.Contains(t, text, `ib_sriov_cni_phase_duration_seconds_count{command="ADD",phase="device_inspection",result="success"} 1`)
	require.NotContains(t, text, "vfio_restore")
	require.NotContains(t, text, "private-tenant")
	require.NotContains(t, text, "0000:03:00.0")
	require.Contains(t, text, `ib_sriov_cni_readiness_total{outcome="first_read_ready"} 1`)
	require.Contains(t, text, `ib_sriov_cni_live_pod_gets_total{reason="cache_miss",result="success"} 1`)
}

func TestFailureClassificationAndBoundedLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		result string
	}{
		{"error", errors.New("secret error"), "error"},
		{"timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{"cancel", context.Canceled, ResultCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMetrics()
			ctx, request := Start(context.Background(), "arbitrary-mode", "arbitrary-command", m)
			Phase(ctx, "arbitrary-phase")(tc.err)
			Readiness(ctx, "arbitrary-outcome")
			LivePodGet(ctx, "arbitrary-reason", tc.err)
			m.Rejected("arbitrary-rejection")
			request.Finish(tc.err)
			require.Equal(t, tc.result, request.Snapshot().Result)
			require.Equal(t, "other", request.Snapshot().ErrorStage)
			text := scrape(t, m)
			require.NotContains(t, text, "arbitrary")
			require.NotContains(t, text, "secret error")
			require.Contains(t, text, `ib_sriov_cni_request_duration_seconds_count{command="other",result="`+tc.result+`"} 1`)
			require.Contains(t, text, `ib_sriov_cni_request_rejections_total{reason="other"} 1`)
		})
	}
}

func TestRequestsAreIsolatedAndSnapshotsImmutable(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			ctx, request := Start(context.Background(), "daemon", "DEL", m)
			pod := fmt.Sprintf("pod-%d", i)
			SetIdentity(ctx, Identity{Pod: pod})
			Phase(ctx, "withdrawal")(nil)
			request.Finish(nil)
			snapshot := request.Snapshot()
			require.Equal(t, pod, snapshot.Identity.Pod)
			snapshot.Phases[0].Name = "mutation"
			require.Equal(t, "withdrawal", request.Snapshot().Phases[0].Name)
		})
	}
	wg.Wait()
	require.Contains(t, scrape(t, m), `ib_sriov_cni_request_duration_seconds_count{command="DEL",result="success"} 20`)
	require.Contains(t, scrape(t, m), `ib_sriov_cni_requests_inflight{command="DEL"} 0`)
}

func TestLogsOnlyAndUnobservedContexts(t *testing.T) {
	ctx := context.Background()
	Phase(ctx, "config_identity")(nil)
	Detail(ctx, "vfio_restore")(nil)
	SetIdentity(ctx, Identity{Pod: "ignored"})
	Readiness(ctx, "first_read_ready")
	LivePodGet(ctx, "cache_miss", nil)
	Patch(ctx, nil, false)
	DiscoveryPoll(ctx)
	require.Equal(t, "standalone", LiveGetReason(ctx))
	require.Equal(t, "cache_miss", LiveGetReason(WithLiveGetReason(ctx, "cache_miss")))
	require.Equal(t, "other", LiveGetReason(WithLiveGetReason(ctx, "private-reason")))
	ctx, request := Start(ctx, "standalone", "ADD", nil)
	request.SetMode("shim")
	Detail(ctx, "vfio_bind")(nil)
	Phase(ctx, "annotation_merge")(errors.New("recovered conflict"))
	request.Finish(nil)
	require.Equal(t, "success", request.Snapshot().Result)
	require.Equal(t, "shim", request.Snapshot().Mode)
	require.Equal(t, "vfio_bind", request.Snapshot().Details[0].Name)
}

func TestGUIDDiscoveryDetailIncludesPollCountWithoutPerPollLogs(t *testing.T) {
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	flags := flag.NewFlagSet("observability-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	require.NoError(t, flags.Set("v", "4"))
	klog.LogToStderr(false)
	var output bytes.Buffer
	klog.SetOutput(&output)
	ctx, request := Start(context.Background(), "daemon", "ADD", nil)
	SetIdentity(ctx, Identity{Pod: "launcher-a", Interface: "ib1", DeviceID: "0000:03:00.0"})
	done := Detail(ctx, "guid_discovery")
	for range 3 {
		DiscoveryPoll(ctx)
	}
	klog.Flush()
	require.Empty(t, output.String(), "individual discovery polls must not log")
	done(nil)
	klog.Flush()
	require.Contains(t, output.String(), `phase="guid_discovery"`)
	require.Contains(t, output.String(), `discoveryPolls=3`)
	require.Contains(t, output.String(), `pod="launcher-a"`)
	require.Contains(t, output.String(), `result="success"`)
	require.Contains(t, output.String(), `elapsed=`)
	request.Finish(nil)
}
