package utils

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// A rewrite that cannot stage its bytes fails before anything is renamed:
// the previous record is exactly as it was and no staging file is left behind.
func TestAFailedRewriteKeepsThePreviousRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	dir := t.TempDir()
	const record = "sandbox-a-ib1"
	previous := []byte(`{"deviceID":"0000:03:00.0","version":1}`)
	if err := saveScratchNetConf(record, dir, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := saveScratchNetConf(record, dir, []byte(`{"deviceID":"0000:03:00.0","version":2}`))
	if err == nil {
		t.Fatal("a rewrite in a directory that admits no new file reported success")
	}
	if !strings.Contains(err.Error(), "failed to create container data") {
		t.Fatalf("failure before the rename reported as %q", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, record))
	if err != nil || !bytes.Equal(got, previous) {
		t.Fatalf("previous record after the failed rewrite: data=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != record {
		t.Fatalf("directory after the failed rewrite: entries=%v err=%v", entries, err)
	}
}

// Once the record is renamed into place, a failure to flush the directory is
// reported as such: the caller learns the record may not survive a crash, and
// finds it in place rather than missing.
func TestAFailureAfterTheRenameLeavesTheRecordInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not bind root")
	}
	dir := t.TempDir()
	const record = "sandbox-a-ib1"
	data := []byte(`{"deviceID":"0000:03:00.0"}`)
	// Writable and searchable but not readable: files can be created and
	// renamed, while the directory itself cannot be opened to be synced.
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := saveScratchNetConf(record, dir, data)
	if err == nil {
		t.Fatal("a directory that cannot be synced reported a durable write")
	}
	if !strings.Contains(err.Error(), "failed to sync the sriov data directory") {
		t.Fatalf("failure after the rename reported as %q", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, record))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("record after the failed sync: data=%q err=%v", got, err)
	}
}

// A DEL reads the record its ADD wrote, possibly while a retry of that ADD
// rewrites it. A reader must see a complete record, the old one or the new
// one, never a truncated or empty file.
func TestReadersNeverSeeAPartialRecord(t *testing.T) {
	dir := t.TempDir()
	const record = "sandbox-a-ib1"
	path := filepath.Join(dir, record)
	const size = 1 << 20
	versions := [][]byte{bytes.Repeat([]byte("a"), size), bytes.Repeat([]byte("b"), size)}
	if err := saveScratchNetConf(record, dir, versions[0]); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var partial []int
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := os.ReadFile(path)
				if err != nil {
					mu.Lock()
					partial = append(partial, -1)
					mu.Unlock()
					return
				}
				if !bytes.Equal(got, versions[0]) && !bytes.Equal(got, versions[1]) {
					mu.Lock()
					partial = append(partial, len(got))
					mu.Unlock()
					return
				}
			}
		}()
	}
	for i := 1; i <= 50; i++ {
		if err := saveScratchNetConf(record, dir, versions[i%2]); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if len(partial) != 0 {
		t.Fatalf("readers saw incomplete records (sizes, -1 = unreadable): %v", partial)
	}
}

// A record name near the filesystem's limit must still be writable: the
// staging name carries only the reserved prefix and a random suffix, never the
// record name, so staging cannot fail where the final name fits.
func TestALongRecordNameStillFitsTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	record := strings.Repeat("c", 250)
	data := []byte(`{"deviceID":"0000:03:00.0"}`)
	if err := saveScratchNetConf(record, dir, data); err != nil {
		t.Fatalf("a %d-byte record name could not be saved: %v", len(record), err)
	}
	got, err := os.ReadFile(filepath.Join(dir, record))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("record after the write: data=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory after the write: entries=%v err=%v", entries, err)
	}
}

// The first record on a node creates the cache directory. The write creates
// every missing level and leaves only the record behind.
func TestTheFirstRecordCreatesTheCacheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "var", "lib", "cni", "ib-sriov")
	const record = "sandbox-a-ib1"
	data := []byte(`{"deviceID":"0000:03:00.0"}`)
	if err := saveScratchNetConf(record, dir, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, record))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("record after the write: data=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != record {
		t.Fatalf("directory after the write: entries=%v err=%v", entries, err)
	}
}
