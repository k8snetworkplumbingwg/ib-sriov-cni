package connectiondetails

import (
	"encoding/json"
	"fmt"
)

const Annotation = "mellanox.infiniband.connection-details"

// Details is keyed by the device index: the PF's rank among the node's
// InfiniBand controllers in sorted-BDF (lspci) order, matching the fabric
// manager's deviceInstance numbering. Keys identify WHICH node PF was
// allocated and are NOT sequential per pod (a single-IB pod may carry only
// "2"); a pod with N IB PFs converges to exactly N entries. Values are PF
// node GUIDs in colon-separated 8-octet form. See the Configuration
// reference in README.md for the design rationale.
type Details map[string]string

// Parse decodes an existing annotation value. An empty value yields an empty
// map so callers treat "no annotation yet" uniformly.
func Parse(existing string) (Details, error) {
	details := Details{}
	if existing == "" {
		return details, nil
	}
	if err := json.Unmarshal([]byte(existing), &details); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Annotation, err)
	}
	return details, nil
}

// IsCurrent reports whether existing already carries a GUID entry for
// indexKey. Used to skip the vfio-pci<->mlx5_core GUID read and the pod
// patch on CNI ADD retries.
func IsCurrent(existing, indexKey string) bool {
	details, err := Parse(existing)
	if err != nil {
		return false
	}
	return details[indexKey] != ""
}

// MergeAnnotation returns the connection-details annotation value with guid set
// at indexKey, preserving existing entries so a pod's interfaces converge as
// each ADD appends its own index. Errors if indexKey is empty or existing is
// not valid connection-details JSON.
func MergeAnnotation(existing, indexKey, guid string) (string, error) {
	if indexKey == "" {
		return "", fmt.Errorf("index key is required")
	}

	details, err := Parse(existing)
	if err != nil {
		return "", err
	}
	details[indexKey] = guid

	encoded, err := json.Marshal(details)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", Annotation, err)
	}
	return string(encoded), nil
}
