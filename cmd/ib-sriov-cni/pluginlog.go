package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"k8s.io/klog/v2"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// The plugin's log is shared by every invocation on the node: the runtime
// execs one process per CNI call, several run at once, and all of them name
// the same file. Each appends with O_APPEND, so lines land whole, and none
// rotates while another may hold the file open: an in-process rotator would
// have one process rename, compress and remove the file another is still
// writing to, and that process's remaining lines would go to an unlinked
// inode. Rotation happens when a process starts and finds the file over its
// size limit: under a lock it renames the file aside and opens a fresh one; a
// process still writing to the renamed file keeps every line, in the backup.
// Backups are pruned by count and age under the same lock, never while young
// enough to be another process's open file, and never compressed. The size is
// checked at start only, so a process never rotates mid-flight and a busy
// node's file may exceed the limit by what the processes it started add. The
// daemon's own log is one process's and rotates in-process.
const (
	megabyte         = 1024 * 1024
	rotateLockName   = ".rotate.lock"
	backupTimeLayout = "2006-01-02T15-04-05.000"
	hoursPerDay      = 24
	// backupGraceMargin is added to the longest readiness wait to bound a
	// plugin process's lifetime: identity reads, device work and the write.
	backupGraceMargin = 30 * time.Minute
)

// backupGrace is longer than any plugin process lives: a backup younger than
// this may still be another process's open file and is left alone. The
// readiness wait is capped at MaxReadinessTimeoutSeconds, so the grace is that
// cap plus a margin for everything else a process does.
const backupGrace = time.Duration(localtypes.MaxReadinessTimeoutSeconds)*time.Second + backupGraceMargin

// openPluginLog opens path for appending, rotating it first when it is over
// maxSizeMB, and prunes its backups. The lock serializes rotation across the
// processes sharing the file; it is released before this returns, so a slow
// write never holds it.
func openPluginLog(path string, maxSizeMB, maxBackups, maxAgeDays int) (*os.File, error) {
	lock := flock.New(filepath.Join(filepath.Dir(path), rotateLockName))
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("log rotation lock: %w", err)
	}
	file, err := rotateAndOpen(path, maxSizeMB, maxBackups, maxAgeDays, time.Now())
	if unlockErr := lock.Unlock(); unlockErr != nil {
		// A lock left held would block every later invocation's log setup;
		// the caller is told, and logs to stderr alone, rather than finding
		// out from the next process. It is reported next to the rotation
		// error when both failed.
		if file != nil {
			_ = file.Close()
		}
		return nil, errors.Join(err, fmt.Errorf("release log rotation lock: %w", unlockErr))
	}
	return file, err
}

// rotateAndOpen is openPluginLog's work under the lock. The backup takes a
// name no backup has: the clock has millisecond resolution and may repeat or
// move back, and a rename over an existing backup would unlink a file another
// process still writes to, before the pruning grace could protect it.
func rotateAndOpen(path string, maxSizeMB, maxBackups, maxAgeDays int, now time.Time) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() >= int64(maxSizeMB)*megabyte {
		at := now
		backup := backupName(path, at)
		for {
			_, err := os.Lstat(backup)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				// Anything but "no such file" is not a taken name; looping
				// on it would hold the rotation lock forever.
				return nil, fmt.Errorf("rotate %s: %w", path, err)
			}
			at = at.Add(time.Millisecond)
			backup = backupName(path, at)
		}
		if err := os.Rename(path, backup); err != nil {
			return nil, fmt.Errorf("rotate %s: %w", path, err)
		}
	}
	pruneBackups(path, maxBackups, maxAgeDays, now)
	// #nosec G304 -- the path is confined by pluginLogPath.
	file, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, pluginLogFileMode)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	return file, nil
}

// backupName is the rotated name of path at the instant at, in the form the
// previous in-process rotator used: <name>-<timestamp><ext>.
func backupName(path string, at time.Time) string {
	dir, file := filepath.Split(path)
	ext := filepath.Ext(file)
	return filepath.Join(dir, strings.TrimSuffix(file, ext)+"-"+at.UTC().Format(backupTimeLayout)+ext)
}

// pruneBackups removes the backups of path beyond maxBackups, newest first by
// the time in their names, and those older than maxAgeDays, except any whose
// modification time is within backupGrace. A failure is logged, not returned:
// a backup that outlives its turn is no reason to fail a CNI call.
func pruneBackups(path string, maxBackups, maxAgeDays int, now time.Time) {
	dir, file := filepath.Split(path)
	ext := filepath.Ext(file)
	prefix := strings.TrimSuffix(file, ext) + "-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		klog.ErrorS(err, "plugin log backups not pruned", "dir", dir)
		return
	}
	type backup struct {
		name     string
		at       time.Time
		modified time.Time
	}
	backups := make([]backup, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		at, err := time.Parse(backupTimeLayout, strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext))
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, backup{name: name, at: at, modified: info.ModTime()})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].at.After(backups[j].at) })
	cutoff := now.Add(-time.Duration(maxAgeDays) * hoursPerDay * time.Hour)
	for i, b := range backups {
		if i < maxBackups && !b.at.Before(cutoff) {
			continue
		}
		if now.Sub(b.modified) < backupGrace {
			continue
		}
		if err := os.Remove(filepath.Join(dir, b.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			klog.ErrorS(err, "plugin log backup not removed", "file", b.name)
		}
	}
}
