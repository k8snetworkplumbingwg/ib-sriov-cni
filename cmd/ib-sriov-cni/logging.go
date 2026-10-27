package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
	"k8s.io/klog/v2"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// Defaults chosen so an unattended node cannot fill its disk. A validated
// image now stays deployed rather than being rolled back, so this log is
// permanent and its bound matters more than its depth.
const (
	defaultLogFileMaxSize    = 100 // megabytes
	defaultLogFileMaxBackups = 3
	defaultLogFileMaxAge     = 7 // days
)

// Caps on the rotation fields. A network definition is tenant input to a root
// process and the host's disk is this plugin's to protect, so a value above a
// cap is reduced to it. Each cap is about an order of magnitude above its
// default: room for a busy node, not for filling a disk.
const (
	maxLogFileMaxSize    = 1024 // megabytes
	maxLogFileMaxBackups = 30
	maxLogFileMaxAge     = 90 // days
)

// setupPluginLogging points this process's log at a node-local file under
// pluginLogDir when the configuration names one.
//
// Only the process the container runtime execs calls this. A CNI plugin is not
// a Pod: the runtime surfaces its stderr only when the call fails, so without a
// file nothing of a successful call's phases is kept. That is true in daemon
// mode too, where this process still resolves its configuration and opens the
// socket, and can fail at either step without the daemon ever hearing about it.
//
// The daemon never calls this. It is a Pod, it logs to stdout, and kubectl logs
// is the right place to read it; redirecting it into a file would take that
// away.
//
// The caller has already parsed the configuration by the time it gets here, so
// a configuration this process cannot read is never in the file. That error
// reaches the runtime instead, which is the one channel that survives without
// it.
func setupPluginLogging(netConf *localtypes.NetConf) {
	if netConf.LogLevel != "" {
		var level klog.Level
		if err := level.Set(netConf.LogLevel); err != nil {
			klog.ErrorS(err, "ignoring logLevel", "logLevel", netConf.LogLevel)
		}
	}
	path, err := pluginLogPath(netConf.LogFile)
	if err != nil {
		klog.ErrorS(err, "ignoring logFile", "logFile", netConf.LogFile)
		return
	}
	if path == "" {
		return
	}
	size, backups, age := boundedRotation(netConf.LogFileMaxSize, netConf.LogFileMaxBackups, netConf.LogFileMaxAge)
	file, err := openPluginLog(path, size, backups, age)
	if err != nil {
		klog.ErrorS(err, "ignoring logFile: it cannot be opened", "logFile", path)
		return
	}
	redirectKlog(file)
}

// pluginLogDir is the one directory the plugin process writes its log under.
// The network definition names the file, never the directory: the plugin runs
// as root on the host, and a namespaced definition author who could choose the
// path would hold an append, rename and truncate primitive on any host file.
//
// It is a directory of its own beneath the daemon's, not the daemon's
// directory itself. Both write host-backed files, and the daemon rotates its
// own in-process: a definition that named the daemon's file would have a
// plugin process rename it aside at start, out from under the daemon's open
// descriptor, and the daemon would go on writing to the renamed file. Nothing
// a definition can say reaches this directory's parent, and the daemon refuses
// to put its own log in here.
const pluginLogDir = "/var/log/ib-sriov-cni/plugin"

// pluginLogRoot is pluginLogDir; tests point it at a directory they own.
var pluginLogRoot = pluginLogDir

// pluginLogDirMode keeps the plugin's log directory to root: its lines name
// Pods and namespaces, and the rotating writer would otherwise create the
// directory world-readable on first use. A directory that already exists is
// tightened to this mode too, whatever made it.
const pluginLogDirMode = 0o700

// pluginLogFileMode keeps the shared log file to root for the same reason.
const pluginLogFileMode = 0o600

// pluginLogPath confines a configured logFile to pluginLogRoot. Only the base
// name of the value is used, so a definition written for the unconfined field
// keeps working and lands in the directory; "." and ".." name no file. A path
// that already exists and is not a regular file, a link or a directory planted
// there, is refused rather than followed. An empty value means no file. The
// directory is created here, root-only, so the first write on a fresh node
// does not decide its mode.
func pluginLogPath(configured string) (string, error) {
	if configured == "" {
		return "", nil
	}
	name := filepath.Base(configured)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return "", fmt.Errorf("%q names no file", configured)
	}
	if err := os.MkdirAll(pluginLogRoot, pluginLogDirMode); err != nil {
		return "", fmt.Errorf("log directory %s: %w", pluginLogRoot, err)
	}
	if err := os.Chmod(pluginLogRoot, pluginLogDirMode); err != nil {
		return "", fmt.Errorf("log directory %s: %w", pluginLogRoot, err)
	}
	path := filepath.Join(pluginLogRoot, name)
	info, err := os.Lstat(path)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return "", fmt.Errorf("%s exists and is not a regular file", path)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	return path, nil
}

// setupDaemonLogging gives the daemon a node-local file of its own, next to the
// plugin's, so both halves of a CNI call can be read on the node the same way.
// Stdout is kept, so kubectl logs is unaffected: the daemon is a Pod and that is
// where an operator looks first. The two never share a path: they run in
// different filesystems, and the daemon's file is the one process's, so it
// rotates in-process; the plugin's file is shared and rotates at process start.
// Rotation is in-process here because there is no logrotate in the deployment,
// and a validated image stays resident, so an unbounded log is a disk-filling bug.
func setupDaemonLogging(opt daemonOptions) {
	if opt.logFile == "" {
		return
	}
	size, backups, age := boundedRotation(opt.logFileMaxSize, opt.logFileMaxBackups, opt.logFileMaxAge)
	redirectKlog(&lumberjack.Logger{
		Filename:   opt.logFile,
		MaxSize:    size,
		MaxBackups: backups,
		MaxAge:     age,
		Compress:   true,
	})
}

// redirectKlog sends klog to w while keeping the original stream: for the
// plugin the runtime surfaces stderr when a call fails, and for the daemon it
// is what kubectl logs reads.
func redirectKlog(w io.Writer) {
	flags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(flags)
	if err := flags.Set("logtostderr", "false"); err != nil {
		klog.ErrorS(err, "klog flag not set", "flag", "logtostderr")
	}
	if err := flags.Set("alsologtostderr", "true"); err != nil {
		klog.ErrorS(err, "klog flag not set", "flag", "alsologtostderr")
	}
	klog.SetOutput(w)
}

// boundedRotation is rotationLimits with a notice for every value it reduced.
func boundedRotation(maxSize, maxBackups, maxAge int) (int, int, int) {
	size, backups, age := rotationLimits(maxSize, maxBackups, maxAge)
	for _, r := range []struct {
		field          string
		asked, applied int
	}{{"logFileMaxSize", maxSize, size}, {"logFileMaxBackups", maxBackups, backups}, {"logFileMaxAge", maxAge, age}} {
		if r.asked > r.applied {
			klog.InfoS("log rotation setting reduced to its cap", "setting", r.field, "requested", r.asked, "cap", r.applied)
		}
	}
	return size, backups, age
}

// rotationLimits is the rotation the log file gets: the configured values
// within their caps, the defaults where nothing positive was configured.
func rotationLimits(maxSize, maxBackups, maxAge int) (int, int, int) {
	return bounded(maxSize, defaultLogFileMaxSize, maxLogFileMaxSize),
		bounded(maxBackups, defaultLogFileMaxBackups, maxLogFileMaxBackups),
		bounded(maxAge, defaultLogFileMaxAge, maxLogFileMaxAge)
}

func bounded(value, fallback, ceiling int) int {
	switch {
	case value <= 0:
		return fallback
	case value > ceiling:
		return ceiling
	}
	return value
}
