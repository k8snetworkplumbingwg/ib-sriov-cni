package connectiondetails

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// StatusAnnotation is written by ib-kubernetes, one member per attachment key,
// and is read-only here. The producer never writes it.
const StatusAnnotation = "v1.ib-sriov-cni.io/connection-status"

// NetworksAnnotation is Multus' network selection, read to resolve which
// attachment this invocation is for. Never written here.
const NetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

// StatusReady is the only value ib-kubernetes publishes. Absence means the
// attachment is still being configured, not that it failed.
const StatusReady = "Ready"

// Status is one member of the status annotation. PfGUID is the GUID the
// controller bound for this attachment, echoed so that a retry after a
// rollback can republish the same allocation without reading the device: the
// details entry is withdrawn by the rollback DEL, and this is the one piece of
// Pod-held state that survives it. Older controllers omit it.
type Status struct {
	Status string `json:"Status"`
	PfGUID string `json:"pfGuid,omitempty"`
}

// Statuses contains the per-attachment status members owned by one Pod.
type Statuses map[string]Status

// ParseStatus decodes the status annotation. An empty value yields an empty
// map so callers treat "not published yet" uniformly. Unknown fields are
// rejected so a future schema cannot be silently misread as readiness.
func ParseStatus(existing string) (Statuses, error) {
	statuses := Statuses{}
	if existing == "" {
		return statuses, nil
	}
	var records map[string]json.RawMessage
	if err := json.Unmarshal([]byte(existing), &records); err != nil {
		return nil, fmt.Errorf("parse %s: %w", StatusAnnotation, err)
	}
	if records == nil {
		return nil, fmt.Errorf("parse %s: expected an object, not null", StatusAnnotation)
	}
	for key, raw := range records {
		var status Status
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&status); err != nil {
			return nil, fmt.Errorf("parse %s entry %q: %w", StatusAnnotation, key, err)
		}
		if status.Status == "" {
			return nil, fmt.Errorf("parse %s entry %q: status is required", StatusAnnotation, key)
		}
		statuses[key] = status
	}
	return statuses, nil
}

// RetainedGUID returns the GUID a Ready status carries for key, if the
// controller echoed one and it would be accepted in a publication. Anything
// else, including a malformed annotation or a placeholder address, is simply
// "unknown": the caller reads the device. The bar is the same one the details
// validator applies, so a value that could not be published is never reused,
// and never remembered, in place of a device read.
func RetainedGUID(existing, key string) (string, bool) {
	statuses, err := ParseStatus(existing)
	if err != nil {
		return "", false
	}
	status, present := statuses[key]
	if !present || status.Status != StatusReady || status.PfGUID == "" {
		return "", false
	}
	guid, err := normalizeGUID(status.PfGUID)
	if err != nil || !strings.EqualFold(guid, status.PfGUID) {
		return "", false
	}
	return strings.ToLower(guid), true
}

// IsReady reports whether the attachment identified by key is Ready.
//
// A value other than Ready is not readiness and is not an error either: only
// Ready is defined, so anything else is an unrecognized state the caller keeps
// waiting through until its deadline. A malformed annotation is an error,
// because an unreadable status cannot be shown to be absent.
func IsReady(existing, key string) (bool, error) {
	statuses, err := ParseStatus(existing)
	if err != nil {
		return false, err
	}
	status, present := statuses[key]
	return present && status.Status == StatusReady, nil
}
