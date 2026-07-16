package main

import (
	"testing"
)

func TestParsePodIdentity(t *testing.T) {
	cases := map[string]struct{ namespace, name string }{
		"IgnoreUnknown=1;K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1": {"ns1", "vm-1"},
		"K8S_POD_NAME=vm-1":         {"", "vm-1"},
		"K8S_POD_NAMESPACE=ns1":     {"ns1", ""},
		"":                          {"", ""},
		"malformed;also-no-equals":  {"", ""},
		"K8S_POD_NAME=vm=extra;X=1": {"", "vm=extra"},
	}
	for in, want := range cases {
		namespace, name := parsePodIdentity(in)
		if namespace != want.namespace || name != want.name {
			t.Fatalf("parsePodIdentity(%q) = (%q, %q), want (%q, %q)",
				in, namespace, name, want.namespace, want.name)
		}
	}
}
