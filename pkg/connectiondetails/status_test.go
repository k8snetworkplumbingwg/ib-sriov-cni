package connectiondetails

import "testing"

const readyStatus = `{"tenant/ib-pf/0000:03:00.0":{"Status":"Ready"}}`

func TestIsReady(t *testing.T) {
	key := "tenant/ib-pf/0000:03:00.0"
	other := "tenant/ib-pf/0002:03:00.0"
	for name, tc := range map[string]struct {
		existing string
		key      string
		want     bool
	}{
		"ready":                {readyStatus, key, true},
		"absent annotation":    {"", key, false},
		"empty object":         {"{}", key, false},
		"sibling only":         {readyStatus, other, false},
		"unrecognized value":   {`{"tenant/ib-pf/0000:03:00.0":{"Status":"Pending"}}`, key, false},
		"case must match":      {`{"tenant/ib-pf/0000:03:00.0":{"Status":"ready"}}`, key, false},
		"ready beside sibling": {`{"tenant/ib-pf/0002:03:00.0":{"Status":"Ready"},"tenant/ib-pf/0000:03:00.0":{"Status":"Ready"}}`, key, true},
		"sibling ready not us": {`{"tenant/ib-pf/0002:03:00.0":{"Status":"Ready"}}`, key, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := IsReady(tc.existing, tc.key)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("IsReady(%s, %q) = %v, want %v", tc.existing, tc.key, got, tc.want)
			}
		})
	}
}

func TestIsReadyRejectsMalformedStatus(t *testing.T) {
	// An unreadable status cannot be shown to be absent, so it must not be
	// silently treated as "not ready yet" and waited out.
	for name, existing := range map[string]string{
		"not json":       "not-json",
		"null":           "null",
		"array":          "[]",
		"member is text": `{"tenant/ib-pf/0000:03:00.0":"Ready"}`,
		"unknown field":  `{"tenant/ib-pf/0000:03:00.0":{"Status":"Ready","Reason":"x"}}`,
		"empty status":   `{"tenant/ib-pf/0000:03:00.0":{"Status":""}}`,
		"missing status": `{"tenant/ib-pf/0000:03:00.0":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := IsReady(existing, "tenant/ib-pf/0000:03:00.0"); err == nil {
				t.Fatalf("IsReady accepted malformed status %s", existing)
			}
		})
	}
}

func TestParseStatusEmpty(t *testing.T) {
	statuses, err := ParseStatus("")
	if err != nil || len(statuses) != 0 {
		t.Fatalf("ParseStatus(\"\") = %v, %v; want an empty map", statuses, err)
	}
}

func TestRetainedGUIDOnlyFromAWellFormedReady(t *testing.T) {
	const key = "ns/net/0000:03:00.0"
	for name, tc := range map[string]struct {
		status string
		want   string
	}{
		"ready with guid":        {`{"` + key + `":{"Status":"Ready","pfGuid":"e0:9d:73:03:00:fe:b0:a0"}}`, "e0:9d:73:03:00:fe:b0:a0"},
		"ready without guid":     {`{"` + key + `":{"Status":"Ready"}}`, ""},
		"not ready":              {`{"` + key + `":{"Status":"Pending","pfGuid":"e0:9d:73:03:00:fe:b0:a0"}}`, ""},
		"malformed guid":         {`{"` + key + `":{"Status":"Ready","pfGuid":"e09d730300feb0a0"}}`, ""},
		"upper-case guid":        {`{"` + key + `":{"Status":"Ready","pfGuid":"E0:9D:73:03:00:FE:B0:A0"}}`, "e0:9d:73:03:00:fe:b0:a0"},
		"placeholder zero guid":  {`{"` + key + `":{"Status":"Ready","pfGuid":"00:00:00:00:00:00:00:00"}}`, ""},
		"placeholder ones guid":  {`{"` + key + `":{"Status":"Ready","pfGuid":"ff:ff:ff:ff:ff:ff:ff:ff"}}`, ""},
		"other key":              {`{"ns/net/0002:03:00.0":{"Status":"Ready","pfGuid":"e0:9d:73:03:00:fe:b0:a0"}}`, ""},
		"unknown field rejected": {`{"` + key + `":{"Status":"Ready","pfGuid":"e0:9d:73:03:00:fe:b0:a0","x":1}}`, ""},
		"empty":                  {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := RetainedGUID(tc.status, key)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("RetainedGUID = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	if ready, err := IsReady(`{"`+key+`":{"Status":"Ready","pfGuid":"e0:9d:73:03:00:fe:b0:a0"}}`, key); err != nil || !ready {
		t.Fatalf("a Ready carrying a GUID must still read as Ready: %v %v", ready, err)
	}
}
