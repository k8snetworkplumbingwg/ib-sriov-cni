package connectiondetails

import (
	"strings"
	"testing"
)

func TestResolveNetwork(t *testing.T) {
	tests := []struct {
		name, annotation, ifName, namespace, network string
	}{
		{"explicit", `[{"name":"ib-pf","namespace":"other","interface":"pod85994b8ab99"}]`, "pod85994b8ab99", "other", "ib-pf"},
		{"default namespace", `[{"name":"ib-pf","interface":"ib0"}]`, "ib0", "tenant", "ib-pf"},
		{"explicit netN", `[{"name":"ib-pf","interface":"net2"}]`, "net2", "tenant", "ib-pf"},
		{"unrelated unnamed selection", `[{"name":"ib-first"},{"name":"ib-second","interface":"net1"}]`, "net1", "tenant", "ib-second"},
		{"same NAD twice", `[{"name":"ib-pf","interface":"ib0"},{"name":"ib-pf","interface":"ib1"}]`, "ib1", "tenant", "ib-pf"},
		{"text explicit", "other/ib-pf@ib0", "ib0", "other", "ib-pf"},
		{"text explicit netN", "ib-first, other/ib-second@net2", "net2", "other", "ib-second"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, network, err := ResolveNetwork("tenant", tt.annotation, tt.ifName, "")
			if err != nil || namespace != tt.namespace || network != tt.network {
				t.Fatalf("ResolveNetwork = %q, %q, %v; want %q, %q", namespace, network, err, tt.namespace, tt.network)
			}
		})
	}
}

func TestResolveNetworkRejectsUnresolvedOrInvalidSelection(t *testing.T) {
	tests := []struct{ name, annotation, ifName string }{
		{"missing", "", "net1"},
		{"empty", "[]", "net1"},
		{"malformed", "[{", "net1"},
		{"null entry", "[null]", "net1"},
		{"missing network name", `[{}]`, "net1"},
		{"no match", `[{"name":"ib-pf","interface":"ib0"}]`, "ib1"},
		{"missing interface", `[{"name":"ib-pf"}]`, ""},
		{"no implicit match on explicit", `[{"name":"ib-pf","interface":"ib0"}]`, "net1"},
		{"ambiguous explicit", `[{"name":"ib-first","interface":"ib0"},{"name":"ib-second","interface":"ib0"}]`, "ib0"},
		{"unnamed single", `[{"name":"ib-pf"}]`, "net1"},
		{"unnamed text", "ib-first, other/ib-second", "net2"},
		{"invalid identity", `[{"name":"other/ib-pf","interface":"ib0"}]`, "ib0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ns, name, err := ResolveNetwork("tenant", tt.annotation, tt.ifName, ""); err == nil {
				t.Fatalf("accepted unresolved/invalid network: %q/%q", ns, name)
			}
		})
	}
}

func TestResolveNetworkDoesNotInferFullDelegateOffset(t *testing.T) {
	// Additional Multus defaultNetworks can put nad-a at net2. Pod selection
	// position alone must not incorrectly identify that invocation as nad-b.
	annotation := `[{"name":"nad-a"},{"name":"nad-b"}]`
	_, _, err := ResolveNetwork("tenant", annotation, "net2", "")
	if err == nil || !strings.Contains(err.Error(), "explicit interface") {
		t.Fatalf("expected error requiring explicit interface, got %v", err)
	}
}

// A selection without an explicit interface is named by Multus from its
// position among all delegates, which the plugin cannot know. What it does
// know is the configuration Multus handed it, whose name the definition's
// owner wrote. Matched against the selections that carry no interface, that
// name identifies the definition without guessing the position.
func TestResolveNetworkIdentifiesAnImplicitInterfaceByTheConfigurationName(t *testing.T) {
	for _, tt := range []struct {
		name, annotation, ifName, configName, namespace, network string
	}{
		{"short form, same definition twice, first", "ib-test/network-a,ib-test/network-a", "net1", "network-a", "ib-test", "network-a"},
		{"short form, same definition twice, second", "ib-test/network-a,ib-test/network-a", "net2", "network-a", "ib-test", "network-a"},
		{"json, no interfaces", `[{"name":"network-a","namespace":"ib-test"},{"name":"network-a","namespace":"ib-test"}]`, "net1", "network-a", "ib-test", "network-a"},
		{"two definitions, position would lie", `[{"name":"nad-a"},{"name":"nad-b"}]`, "net2", "nad-a", "tenant", "nad-a"},
		{"pod namespace by default", "network-a", "net1", "network-a", "tenant", "network-a"},
		{"explicit selection still wins", `[{"name":"a","interface":"ib1"},{"name":"b"}]`, "ib1", "a", "tenant", "a"},
		{"implicit beside an explicit one", `[{"name":"a","interface":"ib1"},{"name":"b"}]`, "net2", "b", "tenant", "b"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			namespace, network, err := ResolveNetwork("tenant", tt.annotation, tt.ifName, tt.configName)
			if err != nil || namespace != tt.namespace || network != tt.network {
				t.Fatalf("ResolveNetwork = %q, %q, %v; want %q, %q", namespace, network, err, tt.namespace, tt.network)
			}
		})
	}
}

// The name is a fallback, not a guess: it never overrides an explicit
// interface, it only considers selections without one, and when it leaves
// more than one definition, or none, the ADD fails and asks for interfaces.
func TestResolveNetworkRefusesWhenTheConfigurationNameDoesNotSettleIt(t *testing.T) {
	for _, tt := range []struct{ name, annotation, ifName, configName string }{
		{"same name in two namespaces", "ns-a/x,ns-b/x", "net1", "x"},
		{"name matches nothing", `[{"name":"a"}]`, "net1", "z"},
		{"only explicit selections", `[{"name":"ib-pf","interface":"ib0"}]`, "net1", "ib-pf"},
		{"no configuration name", `[{"name":"ib-pf"}]`, "net1", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ns, name, err := ResolveNetwork("tenant", tt.annotation, tt.ifName, tt.configName)
			if err == nil {
				t.Fatalf("accepted an unsettled selection: %q/%q", ns, name)
			}
			if !strings.Contains(err.Error(), "explicit interface") {
				t.Fatalf("error does not tell the author what to do: %v", err)
			}
		})
	}
}
