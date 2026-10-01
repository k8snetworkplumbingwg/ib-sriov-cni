package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

const (
	// staleSocketProbeTimeout bounds the liveness probe of an existing socket.
	staleSocketProbeTimeout = 2 * time.Second
	privateDirectoryMode    = 0700
	privateSocketMode       = 0600
	headerTimeout           = 5 * time.Second
	shutdownTimeout         = 5 * time.Second
	// defaultShutdownBudget bounds how long Serve waits, after the HTTP drain,
	// for handlers that are still finishing. A handler mid device rebind needs
	// this time; the Pod's termination grace period must exceed the drain plus
	// this budget, or the kubelet's SIGKILL ends the process regardless.
	defaultShutdownBudget = 30 * time.Second
	handlerPollInterval   = 20 * time.Millisecond
	rejectionUnsupported  = "unsupported"
)

type Server struct {
	listener       *net.UnixListener
	socketInfo     os.FileInfo
	timeout        time.Duration
	shutdownBudget time.Duration
	slots          chan struct{}
	handler        Handler
	metrics        *observability.Metrics
	started        atomic.Bool
	stopping       atomic.Bool
	inFlight       atomic.Int64
}

// ServerOption configures optional node-daemon facilities.
type ServerOption func(*Server)

// WithMetrics adds in-memory metrics to the private CNI request server. It
// does not open a scrape listener or change admission limits.
func WithMetrics(metrics *observability.Metrics) ServerOption {
	return func(server *Server) { server.metrics = metrics }
}

// WithShutdownBudget bounds the wait for handlers still running after the
// drain. Zero or negative keeps the default.
func WithShutdownBudget(budget time.Duration) ServerOption {
	return func(server *Server) {
		if budget > 0 {
			server.shutdownBudget = budget
		}
	}
}

// NewServer requires an existing private directory owned by this daemon's UID.
// It removes a socket nothing listens on, so a restart after an abrupt exit can
// bind, and refuses any other existing path, a live socket included.
func NewServer(socket string, timeout time.Duration, maxInFlight int, handler Handler, options ...ServerOption) (*Server, error) {
	if !filepath.IsAbs(socket) || timeout <= 0 || maxInFlight <= 0 || handler == nil {
		return nil, errors.New("absolute socket, positive limits and handler are required")
	}
	dir, err := os.Lstat(filepath.Dir(socket))
	if err != nil {
		return nil, err
	}
	stat, ok := dir.Sys().(*syscall.Stat_t)
	if !ok || !dir.IsDir() || dir.Mode().Perm() != privateDirectoryMode || int64(stat.Uid) != int64(os.Geteuid()) {
		return nil, errors.New("socket directory must be private (0700) and owned by daemon UID")
	}
	if err := clearStaleSocket(socket); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	info, err := os.Lstat(socket)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	s := &Server{listener: listener, socketInfo: info, timeout: timeout, shutdownBudget: defaultShutdownBudget,
		slots: make(chan struct{}, maxInFlight), handler: handler}
	for _, option := range options {
		option(s)
	}
	if err = os.Chmod(socket, privateSocketMode); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// clearStaleSocket makes a restart possible after a non-graceful exit without
// ever displacing a live server. The socket lives on node-local runtime
// storage that outlives the process, so an abrupt kill leaves the inode
// behind; refusing it unconditionally turns one such kill into a permanent
// crash loop on that node, and every attachment there fails with it.
//
// A socket nobody is listening on refuses connections, and only that case is
// removed. A reachable socket, a path that is not a socket, and any other
// inspection failure are all still refused.
func clearStaleSocket(socket string) error {
	info, err := os.Lstat(socket)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("socket path cannot be inspected")
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("socket path exists and is not a socket")
	}
	conn, err := net.DialTimeout("unix", socket, staleSocketProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return errors.New("socket path is already served by a running daemon")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("socket path exists and could not be probed")
	}
	if err = os.Remove(socket); err != nil {
		return errors.New("stale socket could not be removed")
	}
	return nil
}

// Close removes only the socket inode created by this instance.
func (s *Server) Close() error {
	err := s.listener.Close()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	path := s.listener.Addr().String()
	info, statErr := os.Lstat(path)
	if statErr == nil && os.SameFile(info, s.socketInfo) {
		err = errors.Join(err, os.Remove(path))
	}
	return err
}

// Serve cancels active requests, drains HTTP, then waits for handlers that are
// still finishing, up to the shutdown budget. Handlers must obey context
// cancellation; Go cannot forcibly terminate an arbitrary goroutine. Serve
// returning is what lets the daemon's main exit, so it does not return while
// a handler may still be restoring a device driver, until the budget runs out.
// Past it, the returned error names the handlers still running: a driver bind
// the kernel holds that long is not waited for, and the function may be left
// on the wrong driver until its next ADD.
func (s *Server) Serve(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("server already started")
	}
	defer s.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: headerTimeout,
		ReadTimeout:       s.timeout,
		WriteTimeout:      s.timeout + headerTimeout,
		IdleTimeout:       headerTimeout,
		BaseContext:       func(net.Listener) context.Context { return runCtx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(s.listener) }()
	select {
	case err := <-done:
		cancel()
		_ = server.Close()
		return err
	case <-runCtx.Done():
		s.stopping.Store(true)
		shutdownCtx, stop := context.WithTimeout(context.Background(), shutdownTimeout)
		defer stop()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		<-done
		return errors.Join(err, s.awaitHandlers())
	}
}

// awaitHandlers waits for in-flight handlers to return, up to the shutdown
// budget. Handlers still running past it are reported, not abandoned silently:
// the caller decides whether to exit anyway.
func (s *Server) awaitHandlers() error {
	deadline := time.Now().Add(s.shutdownBudget)
	for s.inFlight.Load() > 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("%d request handler(s) still running %s after shutdown; device state may be unrestored",
				s.inFlight.Load(), s.shutdownBudget)
		}
		time.Sleep(handlerPollInterval)
	}
	return nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != requestPath || r.Method != http.MethodPost {
		s.metrics.Rejected(rejectionUnsupported)
		http.Error(w, "unsupported endpoint", http.StatusNotFound)
		return
	}
	if s.stopping.Load() {
		s.metrics.Rejected("stopping")
		http.Error(w, "daemon shutting down", http.StatusServiceUnavailable)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.metrics.Rejected("capacity")
		http.Error(w, "daemon at capacity", http.StatusServiceUnavailable)
		return
	}
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	deadline := time.Now().Add(s.timeout)
	r.Body = http.MaxBytesReader(w, r.Body, MaxMessageBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		s.metrics.Rejected("malformed")
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		s.metrics.Rejected("malformed")
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.validate() != nil {
		s.metrics.Rejected(rejectionReason(&req))
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Deadline.Before(deadline) {
		deadline = req.Deadline
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	if ctx.Err() != nil {
		s.metrics.Rejected("expired")
		_ = writeResponse(w, failure("request expired"))
		return
	}
	s.serveAccepted(ctx, &req, w)
}

func rejectionReason(req *Request) string {
	if req.Version != ProtocolVersion {
		return rejectionUnsupported
	}
	switch req.Command {
	case AddCommand, DelCommand, CheckCommand:
		return "malformed"
	default:
		return rejectionUnsupported
	}
}

// Only validated, nonexpired requests enter this measurement. Completion is
// observed after both cancellation normalization and the final response write
// attempt, not at the handler's earlier successful return.
func (s *Server) serveAccepted(ctx context.Context, req *Request, w http.ResponseWriter) {
	ctx, observation := observability.Start(ctx, "daemon", req.Command, s.metrics)
	finalErr := errors.New("request did not complete")
	defer func() { observation.Finish(finalErr) }()
	observability.SetIdentity(ctx, observability.Identity{Interface: req.IfName, ContainerID: req.ContainerID})
	response, err := s.handler(ctx, req)
	if err != nil {
		response = failure(fmt.Sprintf("request failed: %v", err))
		response.cause = err
	}
	if ctx.Err() != nil {
		response = failure("request canceled or deadline exceeded")
		response.cause = ctx.Err()
	}
	finalErr = writeResponse(w, response)
}

// writeResponse retains the wire contract while reporting error envelopes and
// failed delivery to the request measurement. HTTP 200 alone is not CNI success.
func writeResponse(w http.ResponseWriter, response *Response) error {
	if response == nil || (response.Error == nil) == (len(response.Result) == 0) {
		response = failure("invalid handler response")
	}
	data, err := json.Marshal(response)
	if err != nil || len(data) > MaxMessageBytes {
		http.Error(w, "invalid or oversized response", http.StatusInternalServerError)
		return errors.New("invalid or oversized response")
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(data); err != nil {
		return err
	}
	if response.Error != nil {
		if response.cause != nil {
			return response.cause
		}
		return response.Error
	}
	return nil
}
