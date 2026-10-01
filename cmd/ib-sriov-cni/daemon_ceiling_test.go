package main

import (
	"testing"
	"time"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// A forwarded ADD may wait the longest readiness timeout a configuration can
// ask for, plus the daemon's call overhead. The default request ceiling must
// cover that, or a valid configuration is cut off by the daemon's own default.
func TestDaemonRequestCeilingCoversTheLongestReadinessWait(t *testing.T) {
	longest := daemonCallOverhead + time.Duration(localtypes.MaxReadinessTimeoutSeconds)*time.Second
	if daemonRequestCeiling < longest {
		t.Fatalf("daemonRequestCeiling %s is below the longest forwarded request %s", daemonRequestCeiling, longest)
	}
}
