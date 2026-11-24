package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/stretchr/testify/mock"
	"k8s.io/klog/v2"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types/mocks"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// The release log is written whether or not the release succeeded, so it has
// to say which: a DEL that failed to release its VF must be told apart from
// one that succeeded by the log alone.
func TestReleaseLogSaysWhetherTheReleaseFailed(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "release failed"}[fails], func(t *testing.T) {
			var logged bytes.Buffer
			state := klog.CaptureState()
			t.Cleanup(state.Restore)
			klog.LogToStderr(false)
			klog.SetOutput(&logged)
			sm := &mocks.Manager{}
			var releaseErr error
			if fails {
				releaseErr = errors.New("release refused")
			}
			sm.On("ReleaseVF", mock.Anything, "ib1", "sandbox-a", mock.Anything).Return(releaseErr)
			sm.On("ResetVFConfig", mock.Anything).Return(nil)
			netConf := &localtypes.NetConf{}
			netConf.DeviceID = "0000:03:00.2"
			args := &skel.CmdArgs{IfName: "ib1", ContainerID: "sandbox-a"}
			err := handleVFCleanup(sm, netConf, args, nil)
			klog.Flush()
			if (err != nil) != fails {
				t.Fatalf("handleVFCleanup = %v, want failure %v", err, fails)
			}
			want := `"device released"`
			if !strings.Contains(logged.String(), want) {
				t.Fatalf("no release log line: %q", logged.String())
			}
			if !strings.Contains(logged.String(), "failed="+map[bool]string{false: "false", true: "true"}[fails]) {
				t.Fatalf("the release log does not say whether the release failed: %q", logged.String())
			}
		})
	}
}

// A DEL that cannot tell whether its device is a virtual function withdraws
// nothing and still succeeds; that leaves a published member in place, so the
// node fault is logged at error level rather than passed over in silence.
func TestADELThatCannotClassifyItsDeviceLogsAnErrorAndSucceeds(t *testing.T) {
	cacheDirAt(t)
	previous := utils.SysBusPci
	utils.SysBusPci = t.TempDir()
	t.Cleanup(func() { utils.SysBusPci = previous })
	// The device's sysfs entry is a file, so the physfn lookup fails with an
	// error that is not "absent".
	if err := os.WriteFile(filepath.Join(utils.SysBusPci, testPF), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	klog.LogToStderr(false)
	klog.SetOutput(&logged)
	called := false
	keep, err := withdrawConnectionDetails(t.Context(), sandboxArgs("sandbox-a"),
		func(*skel.CmdArgs, *localtypes.NetConf) error { called = true; return nil })
	klog.Flush()
	if err != nil || keep || called {
		t.Fatalf("DEL with an unclassifiable device: err=%v keep=%v withdraw called=%v; want success, no record kept, no withdrawal",
			err, keep, called)
	}
	atErrorLevel := false
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.HasPrefix(line, "E") && strings.Contains(line, "withdrawn") && strings.Contains(line, testPF) {
			atErrorLevel = true
		}
	}
	if !atErrorLevel {
		t.Fatalf("no error-level line names the device and the skipped withdrawal: %q", logged.String())
	}
}
