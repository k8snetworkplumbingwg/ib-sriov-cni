package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	labelCommand = "command"
	labelResult  = "result"
	labelReason  = "reason"
)

// Metrics owns a private registry. Construct it once per daemon, not per CNI
// request; no global collectors or short-lived plugin metric servers are used.
type Metrics struct {
	registry  *prometheus.Registry
	duration  *prometheus.HistogramVec
	phase     *prometheus.HistogramVec
	inflight  *prometheus.GaugeVec
	readiness *prometheus.CounterVec
	liveGETs  *prometheus.CounterVec
	rejected  *prometheus.CounterVec
}

// NewMetrics constructs an independent registry with only bounded CNI metrics.
func NewMetrics() *Metrics {
	buckets := []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 240, 600}
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ib_sriov_cni_request_duration_seconds",
			Help: "Accepted daemon request duration from validation to final response attempt in seconds.", Buckets: buckets,
		}, []string{labelCommand, labelResult}),
		phase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ib_sriov_cni_phase_duration_seconds",
			Help: "Completed exclusive CNI phase duration in seconds; nested details are log-only.", Buckets: buckets,
		}, []string{labelCommand, "phase", labelResult}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ib_sriov_cni_requests_inflight", Help: "Validated daemon requests currently being processed.",
		}, []string{labelCommand}),
		readiness: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ib_sriov_cni_readiness_total", Help: "Completed CNI readiness checks by outcome.",
		}, []string{"outcome"}),
		liveGETs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ib_sriov_cni_live_pod_gets_total", Help: "Actual Pod API GET requests, excluding cached and remembered reads.",
		}, []string{labelReason, labelResult}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ib_sriov_cni_request_rejections_total", Help: "Requests rejected before accepted CNI processing starts.",
		}, []string{labelReason}),
	}
	m.registry.MustRegister(m.duration, m.phase, m.inflight, m.readiness, m.liveGETs, m.rejected)
	return m
}

// Handler serves in-memory measurements only; scraping cannot call Kubernetes,
// UFM, or a device. The daemon chooses whether and where to expose it.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Rejected records admission failures separately from completed requests.
func (m *Metrics) Rejected(reason string) {
	if m != nil {
		m.rejected.WithLabelValues(bounded(reason, "unsupported", "malformed", "capacity", "expired", "stopping")).Inc()
	}
}
