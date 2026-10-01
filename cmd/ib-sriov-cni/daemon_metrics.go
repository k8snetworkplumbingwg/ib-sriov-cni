package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

const (
	metricsReadTimeout     = 5 * time.Second
	metricsWriteTimeout    = 10 * time.Second
	metricsIdleTimeout     = 30 * time.Second
	metricsShutdownTimeout = 5 * time.Second
	metricsMaxHeaderBytes  = 8 << 10
)

type metricsEndpoint struct {
	listener net.Listener
	server   *http.Server
	done     chan error
	once     sync.Once
	err      error
}

// startMetricsEndpoint is opt-in and separate from the privileged CNI socket.
// The handler only gathers in-memory collectors. It cannot query Kubernetes,
// inspect a device, or execute a CNI command on behalf of a scrape.
func startMetricsEndpoint(address string, metrics *observability.Metrics) (*metricsEndpoint, error) {
	if address == "" {
		return nil, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	endpoint := &metricsEndpoint{
		listener: listener,
		server: &http.Server{
			Handler: mux, ReadHeaderTimeout: metricsReadTimeout, ReadTimeout: metricsReadTimeout,
			WriteTimeout: metricsWriteTimeout, IdleTimeout: metricsIdleTimeout, MaxHeaderBytes: metricsMaxHeaderBytes,
		},
		done: make(chan error, 1),
	}
	go func() {
		err := endpoint.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		if err != nil {
			// Losing telemetry does not terminate active CNI operations.
			klog.ErrorS(err, "daemon metrics listener stopped")
		}
		endpoint.done <- err
	}()
	klog.InfoS("daemon metrics serving", "address", listener.Addr().String())
	return endpoint, nil
}

func (e *metricsEndpoint) Close() error {
	if e == nil {
		return nil
	}
	e.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
		defer cancel()
		e.err = e.server.Shutdown(ctx)
		if e.err != nil {
			_ = e.server.Close()
		}
		e.err = errors.Join(e.err, <-e.done)
	})
	return e.err
}
