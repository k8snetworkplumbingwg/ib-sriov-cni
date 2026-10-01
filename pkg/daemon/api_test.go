package daemon

import (
	"strings"
	"testing"
	"time"
)

// A request over the socket did not pass through the CNI library's argument
// checks the way an exec'd plugin does, so the daemon applies them itself: the
// container ID and interface name become path elements of node-local files,
// and the library's grammar is what keeps them inside their directories.
func TestRequestValidationEnforcesTheCNIArgumentGrammar(t *testing.T) {
	valid := func() *Request {
		return &Request{Version: ProtocolVersion, Deadline: time.Now().Add(time.Minute), Command: AddCommand,
			ContainerID: "sandbox-1", IfName: "ib1", StdinData: []byte(`{}`)}
	}
	if err := valid().validate(); err != nil {
		t.Fatalf("a well-formed request was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Request){
		"container ID with a path element":   func(r *Request) { r.ContainerID = "../x" },
		"container ID starting with a dot":   func(r *Request) { r.ContainerID = ".tmp-x" },
		"interface name with a path element": func(r *Request) { r.IfName = "../x" },
		"interface name that is a dot":       func(r *Request) { r.IfName = "." },
		"interface name too long":            func(r *Request) { r.IfName = strings.Repeat("i", 16) },
	} {
		r := valid()
		mutate(r)
		if err := r.validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
