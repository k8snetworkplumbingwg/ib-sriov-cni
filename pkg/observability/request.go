// Package observability records request-local CNI timings without exposing
// workload identities as metric labels.
package observability

import (
	"context"
	"errors"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type contextKey struct{}
type liveGetKey struct{}

const (
	// Result labels are shared by logs and metrics and never contain raw errors.
	ResultSuccess   = "success"
	ResultError     = "error"
	ResultTimeout   = "timeout"
	ResultCanceled  = "canceled"
	detailVerbosity = 4
)

// Identity is log-only correlation data, never a source of metric labels.
type Identity struct {
	Namespace, Pod, UID, Interface, DeviceID, ContainerID string
}

// Timing holds one completed parent phase or inclusive debug-only detail.
type Timing struct {
	Name, Result string
	Duration     time.Duration
}

// Snapshot is a copy of the completed measurements at the instant it is read.
type Snapshot struct {
	Mode, Command, Result, ErrorStage                       string
	Identity                                                Identity
	Duration                                                time.Duration
	Phases, Details                                         []Timing
	Readiness                                               string
	LiveGETs, PatchAttempts, PatchConflicts, DiscoveryPolls int
}

// Request belongs to one invocation. Concurrent callbacks are safe, but phase
// ownership and ordering remain the caller's responsibility.
type Request struct {
	mu       sync.Mutex
	started  time.Time
	finished bool
	data     Snapshot
	metrics  *Metrics
}

// Start starts a request measurement. A nil metrics instance records logs only.
func Start(ctx context.Context, mode, command string, metrics *Metrics) (context.Context, *Request) {
	r := &Request{
		started: time.Now(), metrics: metrics,
		data: Snapshot{Mode: bounded(mode, "daemon", "standalone", "shim"), Command: commandLabel(command)},
	}
	if metrics != nil {
		metrics.inflight.WithLabelValues(r.data.Command).Inc()
	}
	return context.WithValue(ctx, contextKey{}, r), r
}

// SetMode updates the log-only execution mode once routing is known.
func (r *Request) SetMode(mode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.data.Mode = bounded(mode, "daemon", "standalone", "shim")
	}
}

// Finish records exactly one completion, after all deferred device restoration
// and the final response attempt. Earlier recovered phase errors do not change
// a successful final result.
func (r *Request) Finish(err error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.data.Duration = time.Since(r.started)
	r.data.Result = resultLabel(err)
	if err == nil {
		r.data.ErrorStage = ""
	} else if r.data.ErrorStage == "" {
		r.data.ErrorStage = "request"
	}
	snapshot := r.snapshotLocked()
	if r.metrics != nil {
		r.metrics.duration.WithLabelValues(snapshot.Command, snapshot.Result).Observe(snapshot.Duration.Seconds())
		r.metrics.inflight.WithLabelValues(snapshot.Command).Dec()
	}
	r.mu.Unlock()
	klog.InfoS("CNI request completed", "mode", snapshot.Mode, "command", snapshot.Command,
		"result", snapshot.Result, "errorStage", snapshot.ErrorStage, "elapsed", snapshot.Duration,
		"namespace", snapshot.Identity.Namespace, "pod", snapshot.Identity.Pod, "podUID", snapshot.Identity.UID,
		"interface", snapshot.Identity.Interface, "deviceID", snapshot.Identity.DeviceID, "containerID", snapshot.Identity.ContainerID,
		"phases", snapshot.Phases, "readiness", snapshot.Readiness, "livePodGETs", snapshot.LiveGETs,
		"patchAttempts", snapshot.PatchAttempts, "patchConflicts", snapshot.PatchConflicts, "discoveryPolls", snapshot.DiscoveryPolls)
}

// Snapshot returns an independent copy; callers cannot mutate live measurements.
func (r *Request) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Request) snapshotLocked() Snapshot {
	data := r.data
	data.Phases = append([]Timing(nil), data.Phases...)
	data.Details = append([]Timing(nil), data.Details...)
	return data
}

func requestFrom(ctx context.Context) *Request {
	r, _ := ctx.Value(contextKey{}).(*Request)
	return r
}

// Phase starts an exclusive top-level stage. The completion callback is safe
// to call early and again from a defer; only its first call is recorded.
func Phase(ctx context.Context, name string) func(error) {
	return timing(ctx, phaseLabel(name), false)
}

// Detail measures an inclusive child stage at debug verbosity only. It never
// contributes to the phase histogram, avoiding parent/child double counting.
func Detail(ctx context.Context, name string) func(error) {
	return timing(ctx, bounded(name, "index_scan", "vfio_unbind", "mlx5_bind", "guid_discovery",
		"mlx5_unbind", "vfio_restore", "vfio_bind", "driver_override"), true)
}

func timing(ctx context.Context, name string, detail bool) func(error) {
	r := requestFrom(ctx)
	if r == nil {
		return func(error) {}
	}
	started := time.Now()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			r.recordTiming(Timing{Name: name, Result: resultLabel(err), Duration: time.Since(started)}, detail)
		})
	}
}

func (r *Request) recordTiming(timing Timing, detail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	if timing.Result != ResultSuccess {
		r.data.ErrorStage = timing.Name
	}
	if detail {
		r.data.Details = append(r.data.Details, timing)
		klog.V(detailVerbosity).InfoS("CNI phase detail", "command", r.data.Command, "namespace", r.data.Identity.Namespace,
			"pod", r.data.Identity.Pod, "interface", r.data.Identity.Interface, "deviceID", r.data.Identity.DeviceID,
			"phase", timing.Name, "result", timing.Result, "elapsed", timing.Duration, "discoveryPolls", r.data.DiscoveryPolls)
		return
	}
	r.data.Phases = append(r.data.Phases, timing)
	if r.metrics != nil {
		r.metrics.phase.WithLabelValues(r.data.Command, timing.Name, timing.Result).Observe(timing.Duration.Seconds())
	}
}

// SetIdentity merges known fields without clearing values resolved earlier.
func SetIdentity(ctx context.Context, identity Identity) {
	r := requestFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	mergeString(&r.data.Identity.Namespace, identity.Namespace)
	mergeString(&r.data.Identity.Pod, identity.Pod)
	mergeString(&r.data.Identity.UID, identity.UID)
	mergeString(&r.data.Identity.Interface, identity.Interface)
	mergeString(&r.data.Identity.DeviceID, identity.DeviceID)
	mergeString(&r.data.Identity.ContainerID, identity.ContainerID)
}

func mergeString(target *string, value string) {
	if value != "" {
		*target = value
	}
}

// Readiness records the final outcome of the request's readiness check once.
func Readiness(ctx context.Context, outcome string) {
	r := requestFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || r.data.Readiness != "" {
		return
	}
	r.data.Readiness = bounded(outcome, "first_read_ready", "waited_ready", ResultTimeout, ResultCanceled, ResultError)
	if r.metrics != nil {
		r.metrics.readiness.WithLabelValues(r.data.Readiness).Inc()
	}
}

// LivePodGet counts an actual API request, never a cache lookup or remembered
// negative result. The operation's caller supplies a fixed reason.
func LivePodGet(ctx context.Context, reason string, err error) {
	r := requestFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.data.LiveGETs++
	if r.metrics != nil {
		r.metrics.liveGETs.WithLabelValues(liveGetLabel(reason), resultLabel(err)).Inc()
	}
}

// Patch records an actual annotation PATCH attempt. Rejected marks an
// optimistic-write conflict; its recovery does not mark the whole request failed.
func Patch(ctx context.Context, _ error, rejected bool) {
	r := requestFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.data.PatchAttempts++
		if rejected {
			r.data.PatchConflicts++
		}
	}
}

// DiscoveryPoll counts sysfs discovery attempts without logging each poll.
func DiscoveryPoll(ctx context.Context) {
	r := requestFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.data.DiscoveryPolls++
	}
}

// WithLiveGetReason tags the reason for a later actual Pod GET.
func WithLiveGetReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, liveGetKey{}, liveGetLabel(reason))
}

// LiveGetReason defaults to standalone when no cache decision tagged the read.
func LiveGetReason(ctx context.Context) string {
	reason, ok := ctx.Value(liveGetKey{}).(string)
	if !ok {
		return "standalone"
	}
	return reason
}

func resultLabel(err error) string {
	switch {
	case err == nil:
		return ResultSuccess
	case errors.Is(err, context.DeadlineExceeded):
		return ResultTimeout
	case errors.Is(err, context.Canceled):
		return ResultCanceled
	default:
		return ResultError
	}
}

func commandLabel(command string) string { return bounded(command, "ADD", "DEL", "CHECK") }

func phaseLabel(name string) string {
	return bounded(name, "config_identity", "device_lock", "ownership_lock", "device_inspection", "guid_cache", "annotation_merge",
		"readiness_wait", "final_lock", "device_finalize", "withdrawal", "cleanup", "transport", "netns_setup")
}

func liveGetLabel(reason string) string {
	return bounded(reason, "cache_miss", "periodic_recheck", "stale_write", "current_confirmation", "standalone")
}

func bounded(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "other"
}
