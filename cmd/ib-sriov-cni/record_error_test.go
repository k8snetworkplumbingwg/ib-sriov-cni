package main

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// The record write's own error stays in the chain, so a caller can tell a
// permission problem from a full disk without parsing the message.
func TestRecordPublicationKeepsTheWriteErrorInTheChain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cacheDirAt(t)
	if err := os.Chmod(config.DefaultCNIDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(config.DefaultCNIDir, 0o700) })
	err := recordPublication(&skel.CmdArgs{ContainerID: "c", IfName: "ib1"}, &localtypes.NetConf{}, publication{UID: "u"}, nil)
	if err == nil {
		t.Fatal("recording into a read-only directory succeeded")
	}
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrPermission) && !errors.As(err, &pathErr) {
		t.Fatalf("the write error is not in the chain: %v", err)
	}
}
