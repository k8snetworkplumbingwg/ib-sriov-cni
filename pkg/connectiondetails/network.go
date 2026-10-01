package connectiondetails

import (
	"encoding/json"
	"fmt"
	"strings"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
)

// ResolveNetwork identifies the NAD selected for this invocation's interface.
//
// A selection that names its interface is matched on it. One that does not is
// named by Multus from its position among every delegate, including defaults
// the Pod annotation never mentions, so the position is not knowable here.
// What is knowable is the configuration Multus passed: the definition's own,
// with the name its owner wrote. Matched against the selections that carry no
// interface, that name identifies the definition without guessing a position.
// When it leaves more than one definition, or none, the ADD fails and asks for
// explicit interfaces.
func ResolveNetwork(podNamespace, networksAnnotation, ifName, configName string) (string, string, error) {
	if ifName == "" {
		return "", "", fmt.Errorf("network interface is required")
	}
	// The upstream parser assumes every JSON selection element is non-null.
	// Check only this shape here; delegate the annotation grammar to it below.
	if strings.HasPrefix(strings.TrimSpace(networksAnnotation), "[") {
		var entries []json.RawMessage
		if err := json.Unmarshal([]byte(networksAnnotation), &entries); err != nil {
			return "", "", fmt.Errorf("parse networks annotation: %w", err)
		}
		for _, entry := range entries {
			if string(entry) == "null" {
				return "", "", fmt.Errorf("networks annotation contains a null selection")
			}
		}
	}
	networks, err := netutils.ParseNetworkAnnotation(networksAnnotation, podNamespace)
	if err != nil {
		return "", "", fmt.Errorf("parse networks annotation: %w", err)
	}
	var namespace, name string
	for _, network := range networks {
		if err := validateNetworkIdentity(network.Namespace, network.Name); err != nil {
			return "", "", err
		}
		if network.InterfaceRequest != ifName {
			continue
		}
		if name != "" {
			return "", "", fmt.Errorf("multiple networks match interface %q", ifName)
		}
		namespace, name = network.Namespace, network.Name
	}
	if name == "" {
		return unnamedSelection(networks, ifName, configName)
	}
	return namespace, name, nil
}

// unnamedSelection identifies the definition behind an interface no selection
// asked for by name: the selections without an interface whose name is the
// configuration's. One namespace means one definition, however many times it
// is selected; more than one, or none, cannot be settled without interfaces.
func unnamedSelection(networks []*nettypes.NetworkSelectionElement, ifName, configName string) (string, string, error) {
	var namespace, name string
	for _, network := range networks {
		if configName == "" || network.InterfaceRequest != "" || network.Name != configName {
			continue
		}
		if name != "" && namespace != network.Namespace {
			return "", "", fmt.Errorf("network %q is selected from more than one namespace without an interface; "+
				"set an explicit interface in the Pod networks annotation", configName)
		}
		namespace, name = network.Namespace, network.Name
	}
	if name == "" {
		return "", "", fmt.Errorf("no network matches interface %q; set an explicit interface in the Pod networks annotation", ifName)
	}
	return namespace, name, nil
}
