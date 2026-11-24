package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
)

// lockHolderEnv names the lock directory a second copy of this test binary
// should take the plugin's lock in. The copy acquires it exactly as an
// invocation does and then waits to be terminated, so the tests below can
// observe what a signal does to a process that holds or waits for the lock.
const lockHolderEnv = "IB_SRIOV_CNI_TEST_LOCK_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(lockHolderEnv); dir != "" {
		config.CniFileLockDir = dir
		fmt.Println("waiting")
		if _, err := lockCNIExecution(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("held")
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	// Node-local state this package writes: the CNI cache and the lock
	// directory. Tests own them here rather than touching the host's paths;
	// individual tests narrow them further.
	dir, err := os.MkdirTemp("", "ib-sriov-cni-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tests need a temporary directory:", err)
		os.Exit(1)
	}
	config.DefaultCNIDir = filepath.Join(dir, "cache")
	config.CniFileLockDir = filepath.Join(dir, "locks")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// A caller that gives up stops waiting for the lock instead of blocking until
// the holder is gone, and the lock is reusable once the holder releases it.
func TestLockAcquisitionStopsWhenTheCallerIsGone(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireLock(context.Background(), dir, "cni.lock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err = acquireLock(ctx, dir, "cni.lock"); err == nil {
		t.Fatal("a second holder acquired the lock")
	}
	if waited := time.Since(started); waited < 100*time.Millisecond {
		t.Fatalf("gave up after %v without waiting for the holder", waited)
	}
	unlockCNIExecution(held)
	released, err := acquireLock(context.Background(), dir, "cni.lock")
	if err != nil {
		t.Fatalf("lock not reusable after release: %v", err)
	}
	unlockCNIExecution(released)
}

// lockHolder runs this test binary as a separate process that takes the lock
// in dir. It returns the process and a reader of its progress lines.
func lockHolder(t *testing.T, dir string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	holder := exec.Command(os.Args[0]) // #nosec G204 -- this test binary
	holder.Env = append(os.Environ(), lockHolderEnv+"="+dir)
	holder.Stderr = os.Stderr
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	return holder, bufio.NewScanner(stdout)
}

// awaitLine fails the test unless the holder prints want next.
func awaitLine(t *testing.T, lines *bufio.Scanner, want string) {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		if lines.Scan() {
			done <- lines.Text()
			return
		}
		done <- fmt.Sprintf("<eof: %v>", lines.Err())
	}()
	select {
	case got := <-done:
		if got != want {
			t.Fatalf("lock holder printed %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("lock holder did not print %q", want)
	}
}

// awaitTermination fails the test unless the process ends by the signal.
func awaitTermination(t *testing.T, process *exec.Cmd, signal syscall.Signal) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("process ended without a signal: %v", err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != signal {
			t.Fatalf("process ended with %v, want termination by %v", exit, signal)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("process survived %v for 5s", signal)
	}
}

// The kernel releases the lock with the process that holds it: a terminated
// holder frees the next invocation without unlocking first, so the plugin
// installs no signal handler and termination keeps its default effect.
func TestTheLockIsReleasedWhenItsHolderIsTerminated(t *testing.T) {
	dir := t.TempDir()
	holder, lines := lockHolder(t, dir)
	awaitLine(t, lines, "waiting")
	awaitLine(t, lines, "held")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := acquireLock(ctx, dir, config.CniFileLockName); err == nil {
		t.Fatal("acquired the lock while another process held it")
	}
	if err := holder.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitTermination(t, holder, syscall.SIGTERM)
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lock, err := acquireLock(ctx, dir, config.CniFileLockName)
	if err != nil {
		t.Fatalf("lock still held after its holder was terminated: %v", err)
	}
	unlockCNIExecution(lock)
}

// An invocation waiting for the lock is still terminated by a signal: the
// bounded wait does not change what a signal does to the process.
func TestAnInvocationWaitingForTheLockIsTerminatedBySignal(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireLock(context.Background(), dir, config.CniFileLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockCNIExecution(held)
	waiter, lines := lockHolder(t, dir)
	awaitLine(t, lines, "waiting")
	// Let the waiter reach its retry loop before interrupting it.
	time.Sleep(3 * cniLockRetryInterval)
	if err := waiter.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	awaitTermination(t, waiter, syscall.SIGTERM)
}

// A lock holder that never returns must not hold every later invocation on
// the node until someone kills them: an acquisition under a context that
// nothing cancels still gives up after the ceiling, and reports the lock.
func TestLockAcquisitionGivesUpAfterTheCeiling(t *testing.T) {
	dir := t.TempDir()
	held, err := acquireLock(context.Background(), dir, config.CniFileLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockCNIExecution(held)
	previous := lockWaitCeiling
	lockWaitCeiling = 200 * time.Millisecond
	t.Cleanup(func() { lockWaitCeiling = previous })
	done := make(chan error, 1)
	go func() {
		_, err := acquireLock(context.Background(), dir, config.CniFileLockName)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("err=%v; the second acquisition must fail naming the lock", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an acquisition under a background context waited past the ceiling")
	}
}
