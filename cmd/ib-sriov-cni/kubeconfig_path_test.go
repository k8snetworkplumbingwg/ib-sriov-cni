package main

import (
	"strings"
	"testing"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
)

// The kubeconfig the plugin loads is the node's, at the fixed path the
// installer writes. A network definition is tenant input to a root process:
// letting it name the file would let an author point the plugin at the
// kubelet's credentials, or at a kubeconfig planted through a Pod volume that
// runs a credential plugin as root.
func TestTheKubeconfigPathIsTheNodesNotTheDefinitions(t *testing.T) {
	netConf, err := config.LoadConf([]byte(`{"name":"net-a","type":"ib-sriov","cniVersion":"0.3.1",` +
		`"deviceID":"0000:03:00.0","ibKubernetesEnabled":true,"publishPfGuid":true,"vfioPciMode":true,` +
		`"kubeconfig":"/etc/kubernetes/admin.conf"}`))
	if err != nil {
		t.Fatal(err)
	}
	err = publishConnectionDetails(t.Context(), sandboxArgs("sandbox-a"), netConf)
	if err == nil {
		t.Fatal("publication succeeded without the node kubeconfig")
	}
	if strings.Contains(err.Error(), "admin.conf") || !strings.Contains(err.Error(), connectiondetails.DefaultKubeconfig) {
		t.Fatalf("the plugin loaded a kubeconfig named by the definition: %v", err)
	}
}
