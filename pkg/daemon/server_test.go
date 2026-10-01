package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/stretchr/testify/require"
)

func startTestServer(t *testing.T, handler Handler, limit int) (*Client, context.CancelFunc, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	socket := filepath.Join(dir, "ib-sriov.sock")
	server, err := NewServer(socket, time.Second, limit, handler)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	client := NewClient(socket)
	t.Cleanup(func() { cancel(); client.Close() })
	return client, cancel, done
}

func testRequest() Request {
	return Request{Command: "ADD", ContainerID: "sandbox-a", Netns: "/run/netns/a", IfName: "ib1",
		Args: "K8S_POD_NAMESPACE=tenant-a;K8S_POD_NAME=launcher-a", Path: "/opt/cni/bin",
		StdinData: []byte(`{"cniVersion":"0.3.1","name":"ib1","type":"ib-sriov"}`)}
}

func TestRequestRoundTrip(t *testing.T) {
	request := testRequest()
	seen := make(chan Request, 1)
	client, _, _ := startTestServer(t, func(_ context.Context, req *Request) (*Response, error) {
		seen <- *req
		return &Response{Result: json.RawMessage(`{"cniVersion":"0.3.1"}`)}, nil
	}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Call(ctx, request)
	require.NoError(t, err)
	require.JSONEq(t, `{"cniVersion":"0.3.1"}`, string(response.Result))
	actual := <-seen
	request.Version = ProtocolVersion
	deadline, _ := ctx.Deadline()
	// The wire carries the deadline as an instant: the decoder restores neither
	// the caller's monotonic reading nor its location, so compare it as one.
	require.True(t, actual.Deadline.Equal(deadline), "deadline %v, want %v", actual.Deadline, deadline)
	actual.Deadline = time.Time{}
	require.Equal(t, request, actual)
}

func TestTypedCNIError(t *testing.T) {
	client, _, _ := startTestServer(t, func(context.Context, *Request) (*Response, error) {
		return &Response{Error: cnitypes.NewError(100, "not implemented", "ADD")}, nil
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.Call(ctx, testRequest())
	require.NoError(t, err)
	require.Equal(t, uint(100), response.Error.Code)
	require.Equal(t, "ADD", response.Error.Details)
}

func TestBlockedCallerDoesNotBlockOtherPod(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	client, stop, done := startTestServer(t, func(ctx context.Context, req *Request) (*Response, error) {
		if req.ContainerID == "sandbox-a" {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}
		return &Response{Result: json.RawMessage(`{"cniVersion":"0.3.1"}`)}, nil
	}, 3)
	ctxA, cancelA := context.WithTimeout(context.Background(), time.Second)
	defer cancelA()
	finishedA := make(chan error, 1)
	go func() { _, err := client.Call(ctxA, testRequest()); finishedA <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("A did not enter")
	}
	ctxB, cancelB := context.WithTimeout(context.Background(), time.Second)
	defer cancelB()
	for _, iface := range []string{"ib1", "ib2"} {
		req := testRequest()
		req.ContainerID, req.IfName = "sandbox-b", iface
		_, err := client.Call(ctxB, req)
		require.NoError(t, err)
	}
	select {
	case <-canceled:
		t.Fatal("A ended before B finished")
	default:
	}
	cancelA()
	require.Error(t, <-finishedA)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect not propagated")
	}
	stop()
	require.NoError(t, <-done)
}

func TestShutdownCancelsHandlerAndRemovesOwnSocket(t *testing.T) {
	entered := make(chan struct{})
	client, stop, done := startTestServer(t, func(ctx context.Context, _ *Request) (*Response, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, 1)
	info, err := os.Stat(client.socket)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _, _ = client.Call(ctx, testRequest()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	stop()
	require.NoError(t, <-done)
	_, err = os.Stat(client.socket)
	require.True(t, errors.Is(err, os.ErrNotExist))
}

func TestSocketRefusesExistingFilesAndUnsafeDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "existing")
	require.NoError(t, os.WriteFile(path, []byte("keep"), 0600))
	_, err := NewServer(path, time.Second, 1, func(context.Context, *Request) (*Response, error) { return nil, nil })
	require.Error(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "keep", string(data))
	require.NoError(t, os.Chmod(dir, 0755))
	_, err = NewServer(filepath.Join(dir, "new.sock"), time.Second, 1,
		func(context.Context, *Request) (*Response, error) { return nil, nil })
	require.Error(t, err)
}

func TestSocketOwnershipAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	socket := filepath.Join(dir, "ib-sriov.sock")
	handler := func(context.Context, *Request) (*Response, error) { return failure("unused"), nil }
	server, err := NewServer(socket, time.Second, 1, handler)
	require.NoError(t, err)
	defer server.Close()
	_, err = NewServer(socket, time.Second, 1, handler)
	require.Error(t, err, "must not replace another live server socket")
	link := filepath.Join(dir, "link.sock")
	require.NoError(t, os.Symlink(socket, link))
	_, err = NewServer(link, time.Second, 1, handler)
	require.Error(t, err)
	moved := filepath.Join(dir, "original.sock")
	require.NoError(t, os.Rename(socket, moved))
	require.NoError(t, os.WriteFile(socket, []byte("replacement"), 0600))
	require.NoError(t, server.Close())
	data, err := os.ReadFile(socket)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(data), "closing must not remove a replacement inode")
}

func TestProtocolInputLimits(t *testing.T) {
	client, _, _ := startTestServer(t, func(context.Context, *Request) (*Response, error) {
		t.Error("invalid request reached handler")
		return nil, nil
	}, 1)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", client.socket)
	}}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: time.Second}
	for _, body := range []string{`{`, `{}`, `{} {}`, `{"unknown":1}`, strings.Repeat("x", MaxMessageBytes+1)} {
		response, err := httpClient.Post("http://unix/v1/request", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
}

func TestClientRequiresDeadlineAndDoesNotRetry(t *testing.T) {
	client := NewClient(filepath.Join(t.TempDir(), "absent.sock"))
	defer client.Close()
	_, err := client.Call(context.Background(), testRequest())
	require.ErrorContains(t, err, "deadline")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = client.Call(ctx, testRequest())
	require.Error(t, err)
}

func TestCapacityAndServerDeadline(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	client, _, _ := startTestServer(t, func(ctx context.Context, _ *Request) (*Response, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan *Response, 1)
	go func() { response, _ := client.Call(ctx, testRequest()); finished <- response }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler not entered")
	}
	_, err := client.Call(ctx, testRequest())
	require.ErrorContains(t, err, "503")
	select {
	case response := <-finished:
		require.NotNil(t, response)
		require.NotNil(t, response.Error, "server deadline must override longer caller deadline")
	case <-ctx.Done():
		t.Fatal("server did not enforce its deadline")
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestClientDoesNotReplayLostResponse(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "broken.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	var calls atomic.Int32
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr == nil {
			_ = conn.Close()
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client := NewClient(socket)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.Call(ctx, testRequest())
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load(), "lost response must not trigger a replay")
}

func TestResponseSizeLimitAndExpiredRequest(t *testing.T) {
	client, _, _ := startTestServer(t, func(context.Context, *Request) (*Response, error) {
		return &Response{Result: json.RawMessage(`"` + strings.Repeat("x", MaxMessageBytes) + `"`)}, nil
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.Call(ctx, testRequest())
	require.ErrorContains(t, err, "500")
	expiredCtx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	_, err = client.Call(expiredCtx, testRequest())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func noopHandler(context.Context, *Request) (*Response, error) { return nil, nil }

// A daemon killed without running its shutdown path leaves the socket inode on
// node-local storage that survives a container restart. Refusing it outright
// turned one such kill into a permanent crash loop on that node, taking every
// attachment on it down until a human deleted the file.
func TestRestartAfterNonGracefulExit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	socket := filepath.Join(dir, "ib-sriov.sock")

	first, err := NewServer(socket, time.Second, 1, noopHandler)
	require.NoError(t, err)
	// Drop the listener without unlinking, exactly as an abrupt kill does.
	require.NoError(t, first.listener.Close())
	_, statErr := os.Lstat(socket)
	require.NoError(t, statErr, "the socket must still be present for this to be the real case")

	second, err := NewServer(socket, time.Second, 1, noopHandler)
	require.NoError(t, err, "a restart must recover from a stale socket")
	t.Cleanup(func() { _ = second.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = second.Serve(ctx) }()
	client := NewClient(socket)
	t.Cleanup(client.Close)
	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("unix", socket, time.Second)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond, "the restarted daemon must accept connections")
}

// Recovering from a stale socket must never displace a daemon that is actually
// running, which is the reason the original code refused unconditionally.
func TestSecondDaemonRefusesALiveSocket(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	socket := filepath.Join(dir, "ib-sriov.sock")

	first, err := NewServer(socket, time.Second, 1, noopHandler)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = first.Serve(ctx) }()

	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("unix", socket, time.Second)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 50*time.Millisecond)

	_, err = NewServer(socket, time.Second, 1, noopHandler)
	require.Error(t, err, "a live socket must not be taken over")
	require.Contains(t, err.Error(), "already served")
	_, statErr := os.Lstat(socket)
	require.NoError(t, statErr, "the live daemon's socket must survive the refusal")
}

// Only a socket is ever removed. Anything else at the path is refused and left
// untouched, so a misconfigured path cannot delete an unrelated file.
func TestNonSocketPathIsRefusedAndPreserved(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "ib-sriov.sock")
	require.NoError(t, os.WriteFile(path, []byte("not a socket"), 0600))

	_, err := NewServer(path, time.Second, 1, noopHandler)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a socket")
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "not a socket", string(data), "a non-socket path must be left alone")
}

// Serve must not return while a handler is still finishing its work after
// cancellation: the daemon's main exits the process when Serve returns, and a
// handler may be restoring a device driver at that moment.
func TestServeJoinsInFlightHandlersBeforeReturning(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan time.Time, 1)
	client, stop, done := startTestServer(t, func(ctx context.Context, _ *Request) (*Response, error) {
		close(entered)
		<-ctx.Done()
		time.Sleep(1500 * time.Millisecond) // the cleanup that cancellation must not skip
		finished <- time.Now()
		return nil, ctx.Err()
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, _ = client.Call(ctx, testRequest()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	stop()
	require.NoError(t, <-done)
	returned := time.Now()
	select {
	case at := <-finished:
		require.False(t, returned.Before(at), "Serve returned %s before the handler finished", at.Sub(returned))
	default:
		t.Fatal("Serve returned while the handler was still running")
	}
}

// A handler that never finishes cannot hold the process forever: past the
// shutdown budget Serve returns an error that names the abandoned work.
func TestServeReportsHandlersStillRunningAfterTheShutdownBudget(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	socket := filepath.Join(dir, "ib-sriov.sock")
	release := make(chan struct{})
	entered := make(chan struct{})
	server, err := NewServer(socket, time.Second, 1, func(ctx context.Context, _ *Request) (*Response, error) {
		close(entered)
		<-release
		return nil, ctx.Err()
	}, WithShutdownBudget(300*time.Millisecond))
	require.NoError(t, err)
	serveCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	client := NewClient(socket)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, _ = client.Call(ctx, testRequest()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	stop()
	err = <-done
	require.Error(t, err)
	require.Contains(t, err.Error(), "still running")
	close(release)
}

// Cleanup that outlives the five-second HTTP drain must still finish before
// Serve returns: the drain deadline error is expected, the early return is not.
func TestServeJoinsCleanupThatOutlivesTheHTTPDrain(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	client, stop, done := startTestServer(t, func(ctx context.Context, _ *Request) (*Response, error) {
		close(entered)
		<-ctx.Done()
		time.Sleep(6 * time.Second)
		close(finished)
		return nil, ctx.Err()
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _, _ = client.Call(ctx, testRequest()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}
	start := time.Now()
	stop()
	err := <-done
	require.ErrorIs(t, err, context.DeadlineExceeded, "HTTP drain budget expired even though cleanup finishes")
	select {
	case <-finished:
	default:
		t.Fatal("Serve returned before cleanup completed after the HTTP drain")
	}
	t.Logf("Serve joined six-second cleanup; returned after %s: %v", time.Since(start), err)
}
