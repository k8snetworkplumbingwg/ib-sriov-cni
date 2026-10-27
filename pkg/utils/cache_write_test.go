package utils

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Observe the real writer's create event, not a reimplementation of its
// naming convention. Events remain queued after the atomic rename completes.
func TestCacheWriteUsesReservedTemporaryPrefix(t *testing.T) {
	dir := t.TempDir()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE); err != nil {
		t.Fatal(err)
	}
	const record = "sandbox-a-ib1"
	const data = `{"deviceID":"0000:03:00.0"}`
	if err := saveScratchNetConf(record, dir, []byte(data)); err != nil {
		t.Fatal(err)
	}
	var events [4096]byte
	n, err := unix.Read(fd, events[:])
	if err != nil {
		t.Fatal(err)
	}
	if n < unix.SizeofInotifyEvent {
		t.Fatalf("no cache-file create event: %d bytes", n)
	}
	nameLen := int(binary.NativeEndian.Uint32(events[12:16]))
	if nameLen == 0 || unix.SizeofInotifyEvent+nameLen > n {
		t.Fatalf("invalid create event name length: %d", nameLen)
	}
	name := strings.TrimRight(string(events[unix.SizeofInotifyEvent:unix.SizeofInotifyEvent+nameLen]), "\x00")
	if !strings.HasPrefix(name, ".tmp-") {
		t.Fatalf("writer created %q; ownership scans require the reserved .tmp- prefix", name)
	}
	got, err := os.ReadFile(filepath.Join(dir, record))
	if err != nil || string(got) != data {
		t.Fatalf("final cache record: data=%q err=%v", got, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != record {
		t.Fatalf("writer left staging files: files=%v err=%v", files, err)
	}
}
