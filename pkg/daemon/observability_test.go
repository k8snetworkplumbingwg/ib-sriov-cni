package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

func metricsText(t *testing.T, metrics *observability.Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.String()
}

func transportRequest(t *testing.T) *http.Request {
	t.Helper()
	req := testRequest()
	req.Version = ProtocolVersion
	req.Deadline = time.Now().Add(time.Minute)
	data, err := json.Marshal(req)
	require.NoError(t, err)
	return httptest.NewRequest(http.MethodPost, requestPath, bytes.NewReader(data))
}

func TestTransportObservesFinalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle Handler
		result string
	}{
		{"success", func(context.Context, *Request) (*Response, error) {
			return &Response{Result: json.RawMessage(`{}`)}, nil
		}, "success"},
		{"error envelope with HTTP 200", func(context.Context, *Request) (*Response, error) {
			return failure("CNI failed"), nil
		}, "error"},
		{"handler error", func(context.Context, *Request) (*Response, error) {
			return nil, errors.New("handler failure")
		}, "error"},
		{"timeout cause survives envelope", func(context.Context, *Request) (*Response, error) {
			return Failure(fmt.Errorf("readiness: %w", context.DeadlineExceeded)), nil
		}, "timeout"},
		{"cancel cause survives envelope", func(context.Context, *Request) (*Response, error) {
			return Failure(context.Canceled), nil
		}, "canceled"},
		{"invalid handler response", func(context.Context, *Request) (*Response, error) { return nil, nil }, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := observability.NewMetrics()
			s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: tc.handle}
			WithMetrics(metrics)(s)
			w := httptest.NewRecorder()
			s.handle(w, transportRequest(t))
			require.Equal(t, http.StatusOK, w.Code)
			text := metricsText(t, metrics)
			require.Contains(t, text, `ib_sriov_cni_request_duration_seconds_count{command="ADD",result="`+tc.result+`"} 1`)
			require.Contains(t, text, `ib_sriov_cni_requests_inflight{command="ADD"} 0`)
			require.NotContains(t, text, "request_rejections_total{")
		})
	}
}

func TestTransportLateCancellationOverridesHandlerSuccess(t *testing.T) {
	metrics := observability.NewMetrics()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: func(context.Context, *Request) (*Response, error) {
		require.Contains(t, metricsText(t, metrics), `ib_sriov_cni_requests_inflight{command="ADD"} 1`)
		cancel()
		return &Response{Result: json.RawMessage(`{}`)}, nil
	}}
	WithMetrics(metrics)(s)
	w := httptest.NewRecorder()
	s.handle(w, transportRequest(t).WithContext(ctx))
	require.Contains(t, w.Body.String(), "request canceled or deadline exceeded")
	require.Contains(t, metricsText(t, metrics), `ib_sriov_cni_request_duration_seconds_count{command="ADD",result="canceled"} 1`)
}

type brokenResponseWriter struct{ header http.Header }

func (w *brokenResponseWriter) Header() http.Header { return w.header }
func (*brokenResponseWriter) WriteHeader(int)       {}
func (*brokenResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("socket delivery failed")
}

func TestTransportResponseWriteFailureIsNotSuccess(t *testing.T) {
	metrics := observability.NewMetrics()
	s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: func(context.Context, *Request) (*Response, error) {
		return &Response{Result: json.RawMessage(`{}`)}, nil
	}}
	WithMetrics(metrics)(s)
	s.handle(&brokenResponseWriter{header: make(http.Header)}, transportRequest(t))
	require.Contains(t, metricsText(t, metrics), `ib_sriov_cni_request_duration_seconds_count{command="ADD",result="error"} 1`)
	require.NotContains(t, metricsText(t, metrics), `result="success"`)
}

func TestTransportWriteFailureOverridesHandlerTimeout(t *testing.T) {
	metrics := observability.NewMetrics()
	s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: func(context.Context, *Request) (*Response, error) {
		return nil, context.DeadlineExceeded
	}}
	WithMetrics(metrics)(s)
	s.handle(&brokenResponseWriter{header: make(http.Header)}, transportRequest(t))
	require.Contains(t, metricsText(t, metrics), `ib_sriov_cni_request_duration_seconds_count{command="ADD",result="error"} 1`)
	require.NotContains(t, metricsText(t, metrics), `result="timeout"`)
}

func TestTransportPanicReleasesInFlightObservation(t *testing.T) {
	metrics := observability.NewMetrics()
	s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: func(context.Context, *Request) (*Response, error) {
		panic("handler panic")
	}}
	WithMetrics(metrics)(s)
	require.Panics(t, func() { s.handle(httptest.NewRecorder(), transportRequest(t)) })
	require.Contains(t, metricsText(t, metrics), `ib_sriov_cni_requests_inflight{command="ADD"} 0`)
	require.Contains(t, metricsText(t, metrics), `ib_sriov_cni_request_duration_seconds_count{command="ADD",result="error"} 1`)
	require.Empty(t, s.slots)
}

func TestTransportRejectionsDoNotCountAsAcceptedRequests(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		request      func(*testing.T) *http.Request
		full         bool
	}{
		{"unsupported endpoint", "unsupported", func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/unexpected", http.NoBody)
		}, false},
		{"malformed JSON", "malformed", func(*testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(`{"unknown":true}`))
		}, false},
		{"unsupported command", "unsupported", func(t *testing.T) *http.Request {
			req := testRequest()
			req.Version, req.Deadline, req.Command = ProtocolVersion, time.Now().Add(time.Minute), "PRIVATE-COMMAND"
			data, err := json.Marshal(req)
			require.NoError(t, err)
			return httptest.NewRequest(http.MethodPost, requestPath, bytes.NewReader(data))
		}, false},
		{"expired", "expired", func(t *testing.T) *http.Request {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return transportRequest(t).WithContext(ctx)
		}, false},
		{"capacity", "capacity", transportRequest, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := observability.NewMetrics()
			s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: func(context.Context, *Request) (*Response, error) {
				t.Fatal("rejected request reached CNI handler")
				return nil, nil
			}}
			WithMetrics(metrics)(s)
			if tc.full {
				s.slots <- struct{}{}
			}
			s.handle(httptest.NewRecorder(), tc.request(t))
			text := metricsText(t, metrics)
			require.Contains(t, text, `ib_sriov_cni_request_rejections_total{reason="`+tc.reason+`"} 1`)
			require.NotContains(t, text, "request_duration_seconds_count{")
			require.NotContains(t, text, "requests_inflight{")
			require.NotContains(t, text, "PRIVATE-COMMAND")
		})
	}
}

func TestMetricsScrapingDoesNotInvokeCNIHandler(t *testing.T) {
	metrics := observability.NewMetrics()
	calls := 0
	s := &Server{timeout: time.Minute, slots: make(chan struct{}, 1), handler: func(context.Context, *Request) (*Response, error) {
		calls++
		return &Response{Result: json.RawMessage(`{}`)}, nil
	}}
	WithMetrics(metrics)(s)
	s.handle(httptest.NewRecorder(), transportRequest(t))
	for range 5 {
		metricsText(t, metrics)
	}
	require.Equal(t, 1, calls)
}
