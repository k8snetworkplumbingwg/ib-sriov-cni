package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

func readAllLogs(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
	}
	return all.String()
}

// The runtime execs one plugin process per CNI call and several run at once,
// all naming the same file. A process that finds the file over its size limit
// when it starts renames it aside and opens a fresh one; a process that is
// still writing to the renamed file keeps every line, in the backup, because
// nothing rewrites or removes a file another process may hold open.
func TestPluginLogRotationKeepsAStaleWritersLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.log")
	first, err := openPluginLog(path, 1, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write(bytes.Repeat([]byte("a"), megabyte)); err != nil {
		t.Fatal(err)
	}
	second, err := openPluginLog(path, 1, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WriteString("STALE-WRITER-LINE\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.WriteString("FRESH-WRITER-LINE\n"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(active, []byte("FRESH-WRITER-LINE")) || bytes.Contains(active, []byte("STALE-WRITER-LINE")) {
		t.Fatalf("active file after rotation: %q", active)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "plugin-*.log"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v; want exactly one", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(backup, []byte("STALE-WRITER-LINE")) {
		t.Fatal("the stale writer's line was lost: it is in neither the active file nor the backup")
	}
}

// Backups are pruned by count and by age, oldest first, except that a backup
// young enough to be another process's open file is left alone: removing it
// would strand that process's remaining lines on an unlinked inode.
func TestPluginLogPrunesBackupsByCountAndAgeExceptYoungOnes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.log")
	now := time.Now()
	ages := map[string]time.Duration{"young": 0, "1d": 24 * time.Hour, "2d": 48 * time.Hour, "3d": 72 * time.Hour, "10d": 240 * time.Hour}
	names := map[string]string{}
	for label, age := range ages {
		name := backupName(path, now.Add(-age))
		if err := os.WriteFile(name, []byte(label), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		names[label] = name
	}
	pruneBackups(path, 2, 7, now)
	for label, want := range map[string]bool{"young": true, "1d": true, "2d": false, "3d": false, "10d": false} {
		_, err := os.Stat(names[label])
		if exists := err == nil; exists != want {
			t.Errorf("backup %s exists=%v, want %v", label, exists, want)
		}
	}
}

// pluginLogHelperEnv tells the test binary to act as a second plugin process:
// open the named log and write one line.
const pluginLogHelperEnv = "IB_SRIOV_CNI_TEST_LOG_WRITER"

// Two real processes: this one has the file open and over its size limit when
// the second opens the same path, rotates it and writes; the first then writes
// through its old descriptor. Both lines must survive somewhere on disk.
func TestPluginLogSurvivesASecondProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.log")
	first, err := openPluginLog(path, 1, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write(bytes.Repeat([]byte("a"), megabyte)); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), pluginLogHelperEnv+"="+path)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("second process: %v\n%s", err, out)
	}
	if _, err := first.WriteString("FIRST-PROCESS-AFTER-ROTATION\n"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	all := readAllLogs(t, dir)
	for _, line := range []string{"SECOND-PROCESS-LINE", "FIRST-PROCESS-AFTER-ROTATION"} {
		if !strings.Contains(all, line) {
			t.Errorf("%s is missing from every file in %s", line, dir)
		}
	}
}

// A backup younger than backupGrace is never removed because a plugin process
// may still write to it, so the grace must outlive the longest such process:
// the readiness wait is bounded by MaxReadinessTimeoutSeconds.
func TestBackupGraceOutlivesTheLongestPluginProcess(t *testing.T) {
	longest := time.Duration(localtypes.MaxReadinessTimeoutSeconds) * time.Second
	if backupGrace <= longest {
		t.Fatalf("backupGrace %v does not outlive a plugin process that may wait %v", backupGrace, longest)
	}
}

// Two rotations may pick the same backup name: the clock has millisecond
// resolution and may repeat or move back. A rename over an existing backup
// would unlink a file another process still writes to, before the pruning
// grace could protect it. Rotation picks a name no backup has.
func TestRotationNeverOverwritesAnExistingBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.log")
	now := time.Date(2026, 10, 6, 1, 2, 3, 4_000_000, time.UTC)
	taken := backupName(path, now)
	if err := os.WriteFile(taken, []byte("EARLIER-BACKUP\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), megabyte), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := rotateAndOpen(path, 1, 3, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	earlier, err := os.ReadFile(taken)
	if err != nil || string(earlier) != "EARLIER-BACKUP\n" {
		t.Fatalf("the existing backup was overwritten: %q %v", earlier, err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "plugin-*.log"))
	if err != nil || len(backups) != 2 {
		t.Fatalf("backups=%v err=%v; want the earlier backup and the new one", backups, err)
	}
}

// A backup name the filesystem refuses, here one past NAME_MAX, must make
// rotation return an error, not loop forever holding the rotation lock while
// every later invocation waits for it.
func TestRotationReturnsALookupErrorInsteadOfLooping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, strings.Repeat("l", 240)+".log")
	if _, err := os.Lstat(backupName(path, time.Now())); !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Skipf("this filesystem does not refuse the long backup name: %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), megabyte), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		file, err := rotateAndOpen(path, 1, 3, 7, time.Now())
		if err == nil {
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, syscall.ENAMETOOLONG) {
			t.Fatalf("rotateAndOpen err=%v; want the lookup error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return within 5 s; it loops on the lookup error")
	}
}
