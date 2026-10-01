package connectiondetails

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const Annotation = "v1.ib-sriov-cni.io/connection-details"

// Detail describes the one PF assigned to a network attachment. DeviceIndex is
// the node-local rank among InfiniBand PCI functions sorted by BDF, not a slot.
type Detail struct {
	NameSpace   string `json:"nameSpace"`
	NetworkName string `json:"networkName"`
	DeviceID    string `json:"deviceID"`
	PfGUID      string `json:"pfGuid"`
	DeviceIndex int    `json:"deviceIndex"`
}

// Key identifies an attachment by NAD namespace/name and assigned PCI BDF.
// Including the BDF preserves repeated attachments of the same NAD.
func (d Detail) Key() string {
	return d.NameSpace + "/" + d.NetworkName + "/" + d.DeviceID
}

// Details contains the attachment records owned by one Pod.
type Details map[string]Detail

var pciBDFPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[01][0-9a-f]\.[0-7]$`)

// ValidateDeviceID accepts only a canonical lower-case PCI BDF. Callers check
// it before the value reaches a sysfs path or an annotation key.
func ValidateDeviceID(deviceID string) error {
	if !pciBDFPattern.MatchString(deviceID) {
		return fmt.Errorf("deviceID %q is not a canonical PCI BDF", deviceID)
	}
	return nil
}

func validateNetworkIdentity(namespace, name string) error {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return fmt.Errorf("invalid NAD namespace/name %q/%q", namespace, name)
	}
	return nil
}

func (d Detail) validate() error {
	if err := validateNetworkIdentity(d.NameSpace, d.NetworkName); err != nil {
		return err
	}
	if err := ValidateDeviceID(d.DeviceID); err != nil {
		return err
	}
	guid, err := normalizeGUID(d.PfGUID)
	if err != nil {
		return fmt.Errorf("invalid pfGuid: %w", err)
	}
	if !strings.EqualFold(guid, d.PfGUID) {
		return fmt.Errorf("pfGuid %q is not an eight-octet GUID", d.PfGUID)
	}
	if d.DeviceIndex < 0 {
		return fmt.Errorf("deviceIndex must be non-negative")
	}
	return nil
}

// Parse decodes an existing annotation value. An empty value yields an empty
// map so callers treat "no annotation yet" uniformly.
func Parse(existing string) (Details, error) {
	details := Details{}
	if existing == "" {
		return details, nil
	}
	var records map[string]json.RawMessage
	if err := json.Unmarshal([]byte(existing), &records); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Annotation, err)
	}
	if records == nil {
		return nil, fmt.Errorf("parse %s: expected an object, not null", Annotation)
	}
	for key, raw := range records {
		var detail Detail
		// A pointer distinguishes a required deviceIndex of zero from an absent
		// or null field. The other required fields cannot be empty.
		type detailFields Detail
		wire := struct {
			*detailFields
			DeviceIndex *int `json:"deviceIndex"`
		}{detailFields: (*detailFields)(&detail)}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&wire); err != nil {
			return nil, fmt.Errorf("parse %s entry %q: %w", Annotation, key, err)
		}
		if wire.DeviceIndex == nil {
			return nil, fmt.Errorf("parse %s entry %q: deviceIndex is required", Annotation, key)
		}
		detail.DeviceIndex = *wire.DeviceIndex
		if err := detail.validate(); err != nil {
			return nil, fmt.Errorf("parse %s entry %q: %w", Annotation, key, err)
		}
		if key != detail.Key() {
			return nil, fmt.Errorf("parse %s: key %q does not match detail key %q", Annotation, key, detail.Key())
		}
		details[key] = detail
	}
	return details, nil
}

// IsCurrent reports whether the exact detail is already published.
func IsCurrent(existing string, detail Detail) bool {
	details, err := Parse(existing)
	if err != nil {
		return false
	}
	current, present := details[detail.Key()]
	return present && current == detail
}

// RemoveAnnotationForDevice removes the member holding deviceID, whatever
// network it was published under, and preserves every sibling.
//
// Withdrawal keys off the allocation rather than the network selection because
// the selection is not stable at teardown: the runtime rewrites the selection
// annotation while a Pod is being deleted, and a DEL that re-resolved the
// network from it would fail to withdraw anything and leave the record behind
// claiming an attachment that no longer exists. A PCI address is assigned to
// one attachment at a time on a node, so it identifies the member exactly.
func RemoveAnnotationForDevice(existing, deviceID string) (string, bool, error) {
	if err := ValidateDeviceID(deviceID); err != nil {
		return "", false, err
	}
	details, err := Parse(existing)
	if err != nil {
		return "", false, err
	}
	changed := false
	for key, detail := range details {
		if detail.DeviceID == deviceID {
			delete(details, key)
			changed = true
		}
	}
	if !changed {
		return existing, false, nil
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return "", false, fmt.Errorf("marshal %s: %w", Annotation, err)
	}
	return string(encoded), true, nil
}

// MergeAnnotation replaces only detail's composite-key member, preserving the
// other delegates' records. Invalid existing annotations are never discarded.
func MergeAnnotation(existing string, detail Detail) (string, error) {
	if err := detail.validate(); err != nil {
		return "", err
	}

	details, err := Parse(existing)
	if err != nil {
		return "", err
	}
	details[detail.Key()] = detail

	encoded, err := json.Marshal(details)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", Annotation, err)
	}
	return string(encoded), nil
}
