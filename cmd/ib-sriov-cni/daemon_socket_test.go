package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A network definition can name any daemonSocketPath, and the plugin dials it
// as root with the CNI request. Before dialing a configured path, the plugin
// requires a Unix socket that the plugin's own user owns, reached without a
// symlink; a missing socket is left to the dial so the usual unreachable
// report is unchanged.
func TestConfiguredSocketMustBeAnOwnedUnixSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := checkConfiguredSocket(socketPath); err != nil {
		t.Fatalf("an owned Unix socket must be accepted: %v", err)
	}
	regular := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkConfiguredSocket(regular); err == nil || !strings.Contains(err.Error(), "not a Unix socket") {
		t.Fatalf("a regular file must be refused as not a socket, got %v", err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(socketPath, link); err != nil {
		t.Fatal(err)
	}
	if err := checkConfiguredSocket(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlink must be refused, got %v", err)
	}
	if err := checkConfiguredSocket("relative.sock"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative path must be refused, got %v", err)
	}
	if err := checkConfiguredSocket(filepath.Join(dir, "absent.sock")); err != nil {
		t.Fatalf("a missing socket is left to the dial, got %v", err)
	}
}
