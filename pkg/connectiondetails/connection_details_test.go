package connectiondetails

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMergeAnnotationCreatesEntry(t *testing.T) {
	value, err := MergeAnnotation("", "0", "94:6d:ae:03:00:33:94:98")
	if err != nil {
		t.Fatalf("MergeAnnotation returned error: %v", err)
	}
	var details Details
	if err := json.Unmarshal([]byte(value), &details); err != nil {
		t.Fatalf("failed to parse connection details: %v", err)
	}
	if details["0"] != "94:6d:ae:03:00:33:94:98" {
		t.Fatalf("unexpected entry: %#v", details)
	}
}

func TestMergeAnnotationAddsEntry(t *testing.T) {
	existing := `{"0":"94:6d:ae:03:00:33:94:98"}`
	value, err := MergeAnnotation(existing, "2", "94:6d:ae:03:00:33:94:99")
	if err != nil {
		t.Fatalf("MergeAnnotation returned error: %v", err)
	}
	var details Details
	if err := json.Unmarshal([]byte(value), &details); err != nil {
		t.Fatalf("failed to parse connection details: %v", err)
	}
	if len(details) != 2 || details["2"] != "94:6d:ae:03:00:33:94:99" {
		t.Fatalf("unexpected details: %#v", details)
	}
}

func TestMergeAnnotationRejectsEmptyKey(t *testing.T) {
	_, err := MergeAnnotation("", "", "94:6d:ae:03:00:33:94:98")
	if err == nil || !strings.Contains(err.Error(), "index key is required") {
		t.Fatalf("expected index key error, got %v", err)
	}
}

func TestIsCurrent(t *testing.T) {
	existing := `{"0":"94:6d:ae:03:00:33:94:98","1":""}`
	if !IsCurrent(existing, "0") {
		t.Fatal("expected current with guid present")
	}
	if IsCurrent(existing, "1") {
		t.Fatal("expected stale for empty guid")
	}
	if IsCurrent(existing, "2") {
		t.Fatal("expected stale for unknown index")
	}
	if IsCurrent("not-json", "0") {
		t.Fatal("expected stale for unparseable annotation")
	}
}
