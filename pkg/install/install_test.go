// Copyright (c) 2026 InfiniBand SR-IOV CNI Authors
// SPDX-License-Identifier: Apache-2.0

package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCopyBinaryInstallsAnIdenticalExecutableAtomically(t *testing.T) {
	src := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho plugin\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst, err := CopyBinary(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	if dst != filepath.Join(dir, BinaryName) {
		t.Fatalf("installed at %q, want %q", dst, filepath.Join(dir, BinaryName))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!/bin/sh\necho plugin\n" {
		t.Fatalf("content changed in transit: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v, want 0755", info.Mode().Perm())
	}
	if _, err := os.Stat(dst + ".temp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	// A second install over the first is the upgrade path and must succeed.
	if _, err := CopyBinary(src, dir); err != nil {
		t.Fatalf("reinstall failed: %v", err)
	}
}

func TestCopyBinaryRefusesAMissingDirectoryOrSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(src, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyBinary(src, filepath.Join(t.TempDir(), "absent")); err == nil ||
		!strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("missing directory accepted or misreported: %v", err)
	}
	if _, err := CopyBinary(filepath.Join(t.TempDir(), "absent"), t.TempDir()); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := CopyBinary(t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("directory accepted as a binary")
	}
}

func TestAPIServerURLBracketsOnlyIPv6(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := APIServerURL(); err == nil {
		t.Fatal("accepted an environment without the service host")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if got, _ := APIServerURL(); got != "https://10.0.0.1:443" {
		t.Fatalf("IPv4: %q", got)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::1")
	if got, _ := APIServerURL(); got != "https://[fd00::1]:443" {
		t.Fatalf("IPv6: %q", got)
	}
}

func serviceAccount(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := ServiceAccountDir
	ServiceAccountDir = dir
	t.Cleanup(func() { ServiceAccountDir = orig })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
}

func TestWriteKubeconfigUsesTheServiceAccountAndPrivateMode(t *testing.T) {
	serviceAccount(t)
	confDir := t.TempDir()
	dst, err := WriteKubeconfig(confDir)
	if err != nil {
		t.Fatal(err)
	}
	if dst != KubeconfigPath(confDir) {
		t.Fatalf("written to %q, want %q", dst, KubeconfigPath(confDir))
	}
	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server: https://10.0.0.1:443", `token: "tok-1"`, "certificate-authority-data: Q0E="} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("kubeconfig lacks %q:\n%s", want, content)
		}
	}
	info, _ := os.Stat(dst)
	if info.Mode().Perm() != kubeconfigFileMode {
		t.Fatalf("mode %v, want %v", info.Mode().Perm(), os.FileMode(kubeconfigFileMode))
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("token-bearing temporary file left behind")
	}
}

func TestWriteKubeconfigRefusesAnUnmountedConfDir(t *testing.T) {
	serviceAccount(t)
	if _, err := WriteKubeconfig(filepath.Join(t.TempDir(), "absent")); err == nil ||
		!strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("unmounted conf dir accepted or misreported: %v", err)
	}
}

func TestRefreshKubeconfigRewritesUntilCanceled(t *testing.T) {
	serviceAccount(t)
	confDir := t.TempDir()
	if _, err := WriteKubeconfig(confDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ServiceAccountDir, "token"), []byte("tok-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); RefreshKubeconfig(ctx, confDir, 10*time.Millisecond, nil) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, _ := os.ReadFile(KubeconfigPath(confDir))
		if strings.Contains(string(content), `token: "tok-2"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotated token never reached the kubeconfig")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh loop did not stop on cancellation")
	}
}

// A source without an execute bit would be installed as it is, and the runtime
// would find a plugin it cannot exec. The copy refuses it instead.
func TestCopyBinaryRefusesANonExecutableSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyBinary(src, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable source accepted or misreported: %v", err)
	}
}

// Two installers can run at once on a node, for instance a terminating
// DaemonSet pod overlapping its replacement. Each must stage its own file, so
// the installed binary is always one writer's complete copy and no staging
// file is left behind.
func TestConcurrentInstallersLeaveAnIntactBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "plugin")
	payload := bytes.Repeat([]byte("ib-sriov plugin bytes\n"), 1<<16)
	if err := os.WriteFile(src, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := CopyBinary(src, dir)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent installer failed: %v", err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, BinaryName))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("installed binary is not one writer's complete copy: len=%d err=%v", len(got), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files left behind: %v err=%v", entries, err)
	}
}

// The same holds for the kubeconfig: concurrent writers never leave a partial
// file in place or a token-bearing staging file behind.
// A staging file holds the service-account token. When the write fails and the
// staging file cannot be removed either, the error names it, so an operator
// knows what is left on the host and why.
func TestAStagingFileThatCannotBeRemovedIsNamedInTheError(t *testing.T) {
	serviceAccount(t)
	confDir := t.TempDir()
	// The destination is a directory with content, so the rename fails.
	if err := os.MkdirAll(filepath.Join(KubeconfigPath(confDir), "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := removeFile
	removeFile = func(string) error { return errors.New("removal refused by the test") }
	t.Cleanup(func() { removeFile = previous })
	_, err := WriteKubeconfig(confDir)
	if err == nil {
		t.Fatal("a rename onto a directory succeeded")
	}
	entries, readErr := os.ReadDir(filepath.Dir(KubeconfigPath(confDir)))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var staging string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			staging = filepath.Join(filepath.Dir(KubeconfigPath(confDir)), entry.Name())
		}
	}
	if staging == "" {
		t.Fatal("the staging file is gone although its removal was refused")
	}
	if !strings.Contains(err.Error(), staging) || !strings.Contains(err.Error(), "removal refused by the test") {
		t.Fatalf("error %q does not name the staging file and the removal failure", err)
	}
}

func TestConcurrentKubeconfigWritersLeaveAnIntactFile(t *testing.T) {
	serviceAccount(t)
	confDir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := WriteKubeconfig(confDir)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent kubeconfig writer failed: %v", err)
		}
	}
	got, err := os.ReadFile(KubeconfigPath(confDir))
	if err != nil || !strings.Contains(string(got), "current-context: ib-sriov-cni-context") {
		t.Fatalf("kubeconfig after concurrent writes: err=%v len=%d", err, len(got))
	}
	entries, err := os.ReadDir(filepath.Dir(KubeconfigPath(confDir)))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files left behind: %v err=%v", entries, err)
	}
}

// The errors WriteKubeconfig returns wrap their cause, so a caller can tell a
// missing service-account token from a missing configuration directory with
// errors.Is instead of parsing text.
func TestWriteKubeconfigErrorsWrapTheirCause(t *testing.T) {
	previous := ServiceAccountDir
	ServiceAccountDir = t.TempDir() // no token, no CA
	t.Cleanup(func() { ServiceAccountDir = previous })
	_, err := WriteKubeconfig(t.TempDir())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing token: %v, want an error wrapping os.ErrNotExist", err)
	}
	serviceAccount(t)
	_, err = WriteKubeconfig(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing conf dir: %v, want an error wrapping os.ErrNotExist", err)
	}
}
