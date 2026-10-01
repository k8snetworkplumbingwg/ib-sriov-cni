// Package daemon carries one CNI command per request over a node-local socket.
// It transports commands; it does not implement them, and it never treats
// cached Pod state as authorization for readiness.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	cniutils "github.com/containernetworking/cni/pkg/utils"
)

const (
	DefaultSocketPath = "/run/ib-sriov-cni/ib-sriov.sock"
	AddCommand        = "ADD"
	DelCommand        = "DEL"
	CheckCommand      = "CHECK"
	ProtocolVersion   = 1
	MaxMessageBytes   = 1 << 20
	requestPath       = "/v1/request"
	internalErrorCode = 100
)

// Request carries only explicitly selected invocation fields, never the process
// environment. Deadline covers transport and handler time without renewal.
type Request struct {
	Version     int       `json:"version"`
	Deadline    time.Time `json:"deadline"`
	Command     string    `json:"command"`
	ContainerID string    `json:"containerID,omitempty"`
	Netns       string    `json:"netns,omitempty"`
	IfName      string    `json:"ifName,omitempty"`
	Args        string    `json:"args,omitempty"`
	Path        string    `json:"path,omitempty"`
	StdinData   []byte    `json:"stdinData,omitempty"`
}

type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cnitypes.Error `json:"error,omitempty"`
	// cause retains wrapped cancellation/deadline identity for local metrics;
	// it is deliberately unexported and absent from the wire protocol.
	cause error
}

// Handler must honor cancellation and return request-local output. It must not
// modify process environment, install signal handlers, or write CNI stdout.
type Handler func(context.Context, *Request) (*Response, error)

func (r *Request) validate() error {
	if r.Version != ProtocolVersion || r.Deadline.IsZero() {
		return errors.New("unsupported protocol or missing deadline")
	}
	switch r.Command {
	case AddCommand, DelCommand, CheckCommand:
		if r.ContainerID == "" || r.IfName == "" || !json.Valid(r.StdinData) {
			return errors.New("CNI request requires container ID, interface and JSON configuration")
		}
		// A request over the socket did not pass the CNI library's argument
		// checks the way an exec'd plugin does; the container ID and interface
		// name become path elements of node-local files, so the library's
		// grammar is applied here too.
		if err := cniutils.ValidateContainerID(r.ContainerID); err != nil {
			return fmt.Errorf("container ID: %w", err)
		}
		if err := cniutils.ValidateInterfaceName(r.IfName); err != nil {
			return fmt.Errorf("interface name: %w", err)
		}
	default:
		return errors.New("unsupported command")
	}
	return nil
}

func failure(message string) *Response {
	return &Response{Error: cnitypes.NewError(internalErrorCode, message, "")}
}

// Failure renders err for the caller. A plugin that already produced a CNI
// error is passed through unchanged, so the runtime reads the original code
// and details rather than a transport-level substitute.
func Failure(err error) *Response {
	var cniErr *cnitypes.Error
	if errors.As(err, &cniErr) {
		return &Response{Error: cniErr, cause: err}
	}
	response := failure(err.Error())
	response.cause = err
	return response
}
