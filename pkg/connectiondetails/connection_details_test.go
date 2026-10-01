package connectiondetails

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAnnotationName(t *testing.T) {
	if Annotation != "v1.ib-sriov-cni.io/connection-details" {
		t.Fatalf("annotation key = %q, want v1.ib-sriov-cni.io/connection-details", Annotation)
	}
}

func testDetail() Detail {
	return Detail{
		NameSpace: "tenant", NetworkName: "ib-pf", DeviceID: "0000:03:00.0",
		PfGUID: "94:6d:ae:03:00:33:94:98", DeviceIndex: 0,
	}
}

func TestMergeAnnotationRoundTrip(t *testing.T) {
	detail := testDetail()
	if got := detail.Key(); got != "tenant/ib-pf/0000:03:00.0" {
		t.Fatalf("Key() = %q", got)
	}
	value, err := MergeAnnotation("", detail)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tenant/ib-pf/0000:03:00.0":{"nameSpace":"tenant","networkName":"ib-pf","deviceID":"0000:03:00.0","pfGuid":"94:6d:ae:03:00:33:94:98","deviceIndex":0}}`
	if value != want {
		t.Fatalf("annotation = %s, want %s", value, want)
	}
	parsed, err := Parse(value)
	if err != nil || !reflect.DeepEqual(parsed, Details{detail.Key(): detail}) {
		t.Fatalf("round trip = %#v, %v", parsed, err)
	}
	if !IsCurrent(value, detail) {
		t.Fatal("identical detail is not current")
	}
	again, err := MergeAnnotation(value, detail)
	if err != nil || again != value {
		t.Fatalf("idempotent merge = %s, %v", again, err)
	}
	changed := detail
	changed.DeviceIndex++
	if IsCurrent(value, changed) {
		t.Fatal("different deviceIndex is current")
	}
}

func TestMergeAnnotationPreservesOtherDevicesAndNetworks(t *testing.T) {
	first := testDetail()
	second := first
	second.DeviceID, second.DeviceIndex = "0002:03:00.0", 1
	third := second
	third.NetworkName, third.DeviceID, third.DeviceIndex = "ib-other", "0010:03:00.0", 2
	want := Details{}
	value := ""
	for _, detail := range []Detail{first, second, third} {
		var err error
		value, err = MergeAnnotation(value, detail)
		if err != nil {
			t.Fatal(err)
		}
		want[detail.Key()] = detail
	}
	got, err := Parse(value)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("merged entries = %#v, %v; want %#v", got, err, want)
	}
	changed := first
	changed.PfGUID, changed.DeviceIndex = "94:6d:ae:03:00:33:94:99", 3
	if IsCurrent(value, changed) {
		t.Fatal("changed detail incorrectly considered current")
	}
	value, err = MergeAnnotation(value, changed)
	if err != nil {
		t.Fatal(err)
	}
	want[changed.Key()] = changed
	got, err = Parse(value)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("replacement = %#v, %v; want %#v", got, err, want)
	}
}

func TestParseRejectsInvalidSchema(t *testing.T) {
	detail := testDetail()
	valid, err := MergeAnnotation("", detail)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"malformed":      "not-json",
		"null":           "null",
		"array":          "[]",
		"flat":           `{"0":"94:6d:ae:03:00:33:94:98"}`,
		"mixed":          strings.TrimSuffix(valid, "}") + `,"1":"94:6d:ae:03:00:33:94:99"}`,
		"null entry":     `{"tenant/ib-pf/0000:03:00.0":null}`,
		"empty entry":    `{"tenant/ib-pf/0000:03:00.0":{}}`,
		"missing index":  strings.Replace(valid, `,"deviceIndex":0`, "", 1),
		"null index":     strings.Replace(valid, `"deviceIndex":0`, `"deviceIndex":null`, 1),
		"negative index": strings.Replace(valid, `"deviceIndex":0`, `"deviceIndex":-1`, 1),
		"fraction index": strings.Replace(valid, `"deviceIndex":0`, `"deviceIndex":0.5`, 1),
		"unknown field":  strings.Replace(valid, `"deviceIndex":0`, `"deviceIndex":0,"sandboxId":"stale"`, 1),
		"namespace key":  strings.Replace(valid, "tenant/ib-pf/", "other/ib-pf/", 1),
		"network key":    strings.Replace(valid, "tenant/ib-pf/", "tenant/other/", 1),
		"device key":     strings.Replace(valid, "tenant/ib-pf/0000:03:00.0", "tenant/ib-pf/0002:03:00.0", 1),
		"missing guid":   strings.Replace(valid, `,"pfGuid":"94:6d:ae:03:00:33:94:98"`, "", 1),
		"malformed guid": strings.Replace(valid, "94:6d:ae:03:00:33:94:98", "bad-guid", 1),
		"zero guid":      strings.Replace(valid, "94:6d:ae:03:00:33:94:98", "00:00:00:00:00:00:00:00", 1),
		"all-ones guid":  strings.Replace(valid, "94:6d:ae:03:00:33:94:98", "ff:ff:ff:ff:ff:ff:ff:ff", 1),
	}
	for name, existing := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(existing); err == nil {
				t.Fatalf("Parse(%s) succeeded", existing)
			}
			if _, err := MergeAnnotation(existing, detail); err == nil {
				t.Fatal("merge silently accepted invalid existing annotation")
			}
			if IsCurrent(existing, detail) {
				t.Fatal("invalid existing annotation is current")
			}
		})
	}
}

func TestValidateDeviceID(t *testing.T) {
	for _, valid := range []string{"0000:03:00.0", "0012:1f:1f.7", "ffff:00:00.0"} {
		if err := ValidateDeviceID(valid); err != nil {
			t.Fatalf("ValidateDeviceID(%q) = %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"", "03:00.0", "0000:03:00.8", "0000:03:20.0", "0000:03:00.0\n", "0000:03:00.0 ",
		"0000:03:00.0/../x", "0000:03:00.G", "0000:03:00.0;rm",
	} {
		if err := ValidateDeviceID(invalid); err == nil {
			t.Fatalf("ValidateDeviceID(%q) accepted a non-canonical BDF", invalid)
		}
	}
}

func TestMergeAnnotationRejectsInvalidDetail(t *testing.T) {
	for _, field := range []string{"nameSpace", "networkName", "deviceID", "pfGuid", "deviceIndex"} {
		t.Run(field, func(t *testing.T) {
			detail := testDetail()
			switch field {
			case "nameSpace":
				detail.NameSpace = ""
			case "networkName":
				detail.NetworkName = "ns/nad"
			case "deviceID":
				detail.DeviceID = "3:0.0"
			case "pfGuid":
				detail.PfGUID = "bad"
			case "deviceIndex":
				detail.DeviceIndex = -1
			}
			if _, err := MergeAnnotation("", detail); err == nil {
				t.Fatalf("accepted invalid %s: %#v", field, detail)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	for _, value := range []string{"", "{}"} {
		got, err := Parse(value)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("Parse(%q) = %#v, %v", value, got, err)
		}
	}
	if IsCurrent("", testDetail()) {
		t.Fatal("missing entry is current")
	}
}

func TestRemoveAnnotationForDeviceRemovesOnlyItsOwnMember(t *testing.T) {
	mine := testDetail()
	sibling := mine
	sibling.DeviceID = "0002:03:00.0"
	both, err := MergeAnnotation(mustMerge(t, "", mine), sibling)
	if err != nil {
		t.Fatal(err)
	}

	value, changed, err := RemoveAnnotationForDevice(both, mine.DeviceID)
	if err != nil || !changed {
		t.Fatalf("RemoveAnnotationForDevice = %q, %v, %v", value, changed, err)
	}
	details, err := Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := details[mine.Key()]; present {
		t.Fatal("own member survived removal")
	}
	if got, present := details[sibling.Key()]; !present || got != sibling {
		t.Fatalf("sibling not preserved: %+v", details)
	}
}

func TestRemoveAnnotationForDeviceIsANoOpWhenNothingIsOwned(t *testing.T) {
	mine := testDetail()
	for name, tc := range map[string]struct{ existing, deviceID string }{
		"absent annotation": {"", mine.DeviceID},
		"empty object":      {"{}", mine.DeviceID},
		"different device":  {mustMerge(t, "", mine), "0002:03:00.0"},
	} {
		t.Run(name, func(t *testing.T) {
			value, changed, err := RemoveAnnotationForDevice(tc.existing, tc.deviceID)
			if err != nil || changed || value != tc.existing {
				t.Fatalf("RemoveAnnotationForDevice = %q, %v, %v; want the input returned unchanged",
					value, changed, err)
			}
		})
	}
}

func TestRemoveAnnotationForDeviceRefusesUnreadableData(t *testing.T) {
	// Overwriting an annotation we cannot read would discard another writer's
	// members, so removal must fail instead.
	for _, existing := range []string{"not-json", "null", "[]", `{"tenant/ib-pf/0000:03:00.0":{}}`} {
		if _, _, err := RemoveAnnotationForDevice(existing, "0000:03:00.0"); err == nil {
			t.Fatalf("RemoveAnnotationForDevice accepted unreadable annotation %s", existing)
		}
	}
}

func mustMerge(t *testing.T, existing string, detail Detail) string {
	t.Helper()
	value, err := MergeAnnotation(existing, detail)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRemoveAnnotationForDeviceIgnoresTheNetworkSelection(t *testing.T) {
	mine := Detail{NameSpace: "ns1", NetworkName: "net-a", DeviceID: "0000:03:00.0",
		PfGUID: "90:e3:17:03:00:fb:91:8e", DeviceIndex: 0}
	other := Detail{NameSpace: "ns1", NetworkName: "net-b", DeviceID: "0002:03:00.0",
		PfGUID: "90:e3:17:03:00:fb:91:8f", DeviceIndex: 1}
	encoded, err := json.Marshal(Details{mine.Key(): mine, other.Key(): other})
	if err != nil {
		t.Fatal(err)
	}
	// The caller knows only the PCI address at teardown; the selection the
	// entry was published under is not available and must not be needed.
	value, changed, err := RemoveAnnotationForDevice(string(encoded), mine.DeviceID)
	if err != nil || !changed {
		t.Fatalf("remove = (%v, %v)", changed, err)
	}
	left, err := Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[other.Key()] != other {
		t.Fatalf("remaining = %+v, want only %+v", left, other)
	}
	if _, changed, err = RemoveAnnotationForDevice(value, mine.DeviceID); err != nil || changed {
		t.Fatalf("second removal = (%v, %v), want no change", changed, err)
	}
	if _, _, err = RemoveAnnotationForDevice(value, "not-a-bdf"); err == nil {
		t.Fatal("malformed device accepted")
	}
	if _, _, err = RemoveAnnotationForDevice("{bad", mine.DeviceID); err == nil {
		t.Fatal("unreadable annotation accepted")
	}
}
