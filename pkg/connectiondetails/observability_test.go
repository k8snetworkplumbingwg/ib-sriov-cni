package connectiondetails

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

func scrapeObservations(metrics *observability.Metrics) string {
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	return w.Body.String()
}

func TestReadyObservationsPreserveReadCountAndOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		readyAt       int
		failure       error
	}{
		{name: "retained", outcome: "first_read_ready", readyAt: 1},
		{name: "converged", outcome: "waited_ready", readyAt: 2},
		{name: "error", outcome: "error", failure: errors.New("read failed")},
		{name: "canceled", outcome: "canceled", failure: context.Canceled},
		{name: "timeout", outcome: "timeout", failure: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := observability.NewMetrics()
			ctx, request := observability.Start(t.Context(), "daemon", "ADD", metrics)
			reads := 0
			getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
				reads++
				if tc.failure != nil {
					return nil, tc.failure
				}
				if reads < tc.readyAt {
					return podWithStatus("uid-1", "{}"), nil
				}
				return podWithStatus("uid-1", readyStatus), nil
			})
			err := WaitForReady(ctx, getter, "tenant", "vm-1", "uid-1", waitKey)
			request.Finish(err)
			if !errors.Is(err, tc.failure) {
				t.Fatalf("error changed: %v", err)
			}
			wantReads := max(tc.readyAt, 1)
			if reads != wantReads {
				t.Fatalf("reads=%d, want %d", reads, wantReads)
			}
			want := `ib_sriov_cni_readiness_total{outcome="` + tc.outcome + `"} 1`
			if got := scrapeObservations(metrics); !strings.Contains(got, want) {
				t.Fatalf("missing %s in metrics:\n%s", want, got)
			}
		})
	}
}

func TestLiveGetObservationCountsOnlyRealClientCalls(t *testing.T) {
	kube := fake.NewClientset(podWithStatus("uid-1", readyStatus))
	client := NewPodClientForClientset(kube)
	metrics := observability.NewMetrics()
	ctx, request := observability.Start(t.Context(), "daemon", "ADD", metrics)
	ctx = observability.WithLiveGetReason(ctx, "periodic_recheck")
	if _, err := client.GetPod(ctx, "tenant", "vm-1"); err != nil {
		t.Fatal(err)
	}
	request.Finish(nil)
	for range 3 {
		if got := scrapeObservations(metrics); !strings.Contains(got,
			`ib_sriov_cni_live_pod_gets_total{reason="periodic_recheck",result="success"} 1`) {
			t.Fatalf("unexpected GET measurements: %s", got)
		}
	}
	if len(kube.Actions()) != 1 {
		t.Fatalf("scrapes caused API work: %v", kube.Actions())
	}
}

func TestGUIDObservationIncludesDeferredRestore(t *testing.T) {
	for _, failRestore := range []bool{false, true} {
		t.Run(map[bool]string{false: "restored", true: "restore_error"}[failRestore], func(t *testing.T) {
			f := fakeSysfs(t)
			if failRestore {
				f.failBindTo = vfioPCIDriver
			}
			ctx, request := observability.Start(t.Context(), "daemon", "ADD", nil)
			finish := observability.Phase(ctx, "device_inspection")
			guid, err := ReadPFNodeGUIDObserved(ctx, testBDF)
			finish(err)
			request.Finish(err)
			if (err != nil) != failRestore || (guid == "") != failRestore {
				t.Fatalf("guid=%q err=%v restoreFailure=%v", guid, err, failRestore)
			}
			snapshot := request.Snapshot()
			if snapshot.DiscoveryPolls != 1 {
				t.Fatalf("discovery attempts=%d, want 1", snapshot.DiscoveryPolls)
			}
			foundRestore := false
			for _, detail := range snapshot.Details {
				if detail.Name == "vfio_restore" {
					foundRestore = true
					want := "success"
					if failRestore {
						want = "error"
					}
					if detail.Result != want || detail.Duration < 0*time.Second {
						t.Fatalf("restore observation: %+v", detail)
					}
				}
			}
			if !foundRestore {
				t.Fatal("completion omitted deferred restore")
			}
			if !failRestore {
				f.expect(t, vfioPCIDriver, "vfio-pci/unbind", "mlx5_core/bind", "mlx5_core/unbind", "vfio-pci/bind")
			}
		})
	}
}

func TestReadyObservationRecordsActualContextExpiry(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "timeout"}[deadline], func(t *testing.T) {
			metrics := observability.NewMetrics()
			ctx, request := observability.Start(t.Context(), "daemon", "ADD", metrics)
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			getter := getterFunc(func(context.Context, string, string) (*corev1.Pod, error) {
				if !deadline {
					cancel()
				}
				return podWithStatus("uid-1", "{}"), nil
			})
			err := WaitForReady(ctx, getter, "tenant", "vm-1", "uid-1", waitKey)
			request.Finish(err)
			if !errors.Is(err, ctx.Err()) || err == nil {
				t.Fatalf("error=%v, ctx=%v", err, ctx.Err())
			}
			want := "canceled"
			if deadline {
				want = "timeout"
			}
			if request.Snapshot().Readiness != want || request.Snapshot().Result != want {
				t.Fatalf("expiry observation=%+v", request.Snapshot())
			}
		})
	}
}

func TestGUIDObservationsOnBindAndDiscoveryFailures(t *testing.T) {
	for _, tc := range []struct {
		name, failingBind, badGUID, failingDetail string
		polls                                     int
	}{
		{name: "mlx5_bind", failingBind: mlx5CoreDriver, failingDetail: "mlx5_bind"},
		{name: "invalid_guid", badGUID: "invalid", failingDetail: "guid_discovery", polls: 1},
		{name: "discovery_and_restore", badGUID: "invalid", failingBind: vfioPCIDriver, failingDetail: "vfio_restore", polls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeSysfs(t)
			f.failBindTo = tc.failingBind
			if tc.badGUID != "" {
				path := filepath.Join(f.devices, testBDF, "infiniband", "mlx5_0", "node_guid")
				if err := os.WriteFile(path, []byte(tc.badGUID), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, request := observability.Start(t.Context(), "daemon", "ADD", nil)
			finish := observability.Phase(ctx, "device_inspection")
			guid, err := ReadPFNodeGUIDObserved(ctx, testBDF)
			finish(err)
			request.Finish(err)
			if err == nil || guid != "" {
				t.Fatalf("guid=%q err=%v", guid, err)
			}
			assertGUIDFailureObservation(t, request.Snapshot(), tc.polls, tc.failingDetail)
			if tc.failingBind != vfioPCIDriver {
				if driver, err := getPCIDeviceDriver(testBDF); err != nil || driver != vfioPCIDriver {
					t.Fatalf("not restored: driver=%s err=%v", driver, err)
				}
			}
		})
	}
}

func assertGUIDFailureObservation(t *testing.T, snapshot observability.Snapshot, polls int, failingDetail string) {
	t.Helper()
	if snapshot.DiscoveryPolls != polls || snapshot.Result != "error" || snapshot.Phases[0].Result != "error" {
		t.Fatalf("failure observation=%+v", snapshot)
	}
	var foundFailure, foundRestore bool
	for _, detail := range snapshot.Details {
		foundFailure = foundFailure || (detail.Name == failingDetail && detail.Result == "error")
		foundRestore = foundRestore || detail.Name == "vfio_restore"
	}
	if !foundFailure || !foundRestore {
		t.Fatalf("missing failure or restore: %+v", snapshot.Details)
	}
}
