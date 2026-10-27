package main

import (
	"flag"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

func TestMetricsListenerDefaultsDisabled(t *testing.T) {
	opt := registerDaemonFlags(flag.NewFlagSet("test", flag.ContinueOnError))
	if opt.metricsBindAddress != "" {
		t.Fatalf("metrics opened by default: %s", opt.metricsBindAddress)
	}
	endpoint, err := startMetricsEndpoint("", nil)
	if err != nil || endpoint != nil {
		t.Fatalf("disabled endpoint=%v err=%v", endpoint, err)
	}
}

func TestMetricsListenerServesOnlyMetricsAndCloses(t *testing.T) {
	metrics := observability.NewMetrics()
	metrics.Rejected("capacity")
	endpoint, err := startMetricsEndpoint("127.0.0.1:0", metrics)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endpoint.Close() })
	address := endpoint.listener.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}
	for range 3 {
		response, getErr := client.Get("http://" + address + "/metrics")
		if getErr != nil {
			t.Fatal(getErr)
		}
		data, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK ||
			!strings.Contains(string(data), `ib_sriov_cni_request_rejections_total{reason="capacity"} 1`) {
			t.Fatalf("scrape status=%d body=%s err=%v", response.StatusCode, data, readErr)
		}
	}
	response, err := client.Post("http://"+address+"/cni", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("metrics listener accepted CNI route: %d", response.StatusCode)
	}
	if err = endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("metrics listener survived close")
	}
}

func TestMetricsListenerReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if endpoint, err := startMetricsEndpoint(occupied.Addr().String(), observability.NewMetrics()); err == nil {
		_ = endpoint.Close()
		t.Fatal("occupied metrics address accepted")
	}
}
