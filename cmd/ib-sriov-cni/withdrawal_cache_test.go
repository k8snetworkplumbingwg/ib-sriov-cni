package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

func withdrawalCacheTest(t *testing.T) (*annotationClientFake, *skel.CmdArgs, connectiondetails.Detail) {
	t.Helper()
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	args := sandboxArgs("sandbox-a")
	args.Netns = filepath.Join(t.TempDir(), "gone-netns")
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publishRecorded(t.Context(), f, args, conf,
		func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	f.pod.Annotations[connectiondetails.StatusAnnotation] = `{"ns1/net-a/0000:03:00.0":{"Status":"Ready"}}`
	return f, args, mine
}

func TestDELTemporaryCacheFilesDoNotSuppressWithdrawal(t *testing.T) {
	for _, state := range []string{"empty", "partial", "complete"} {
		t.Run(state, func(t *testing.T) {
			f, args, mine := withdrawalCacheTest(t)
			sibling := annotationEntry("net-b", "0002:03:00.0", 1)
			f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{
				"ns1/net-a/0000:03:00.0": mine,
				"ns1/net-b/0002:03:00.0": sibling,
			})
			status := f.pod.Annotations[connectiondetails.StatusAnnotation]
			// Model a writer paused before rename, including a complete but
			// uncommitted later record. None of these files owns a publication.
			var data []byte
			switch state {
			case "partial":
				data = []byte("{")
			case "complete":
				other := *mustRecord(t, args)
				other.PublishedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				var err error
				data, err = json.Marshal(other)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(config.DefaultCNIDir, ".tmp-sandbox-b-ib1-123"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := delNetwork(t.Context(), args, standalone(withdrawer(f))); err != nil {
				t.Fatal(err)
			}
			checkEntry(t, f, sibling, 1)
			if f.pod.Annotations[connectiondetails.StatusAnnotation] != status {
				t.Fatal("DEL changed retained readiness status")
			}
			if _, err := readRecordedPublication(args); !errors.Is(err, errNoRecord) {
				t.Fatalf("successful DEL left its ownership record: %v", err)
			}
		})
	}
}

func TestDELIndeterminateSiblingKeepsOwnRecordForRetry(t *testing.T) {
	for _, emptyNetns := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-netns", true: "empty-netns"}[emptyNetns], func(t *testing.T) {
			f, args, mine := withdrawalCacheTest(t)
			if emptyNetns {
				args.Netns = ""
			}
			status := f.pod.Annotations[connectiondetails.StatusAnnotation]
			// A sibling record of the same attachment whose instant cannot be
			// read: it may be the live sandbox's, so ownership is unsettled.
			sibling := *mustRecord(t, args)
			sibling.PublishedAt = "not an instant"
			if err := utils.SaveNetConf("sandbox-b", config.DefaultCNIDir, "ib1", &sibling); err != nil {
				t.Fatal(err)
			}
			patches := f.patches
			if err := delNetwork(t.Context(), args, standalone(withdrawer(f))); err != nil {
				t.Fatal(err)
			}
			if _, err := readRecordedPublication(args); err != nil {
				t.Fatalf("indeterminate withdrawal discarded retry ownership: %v", err)
			}
			checkEntry(t, f, mine, 1)
			if f.patches != patches {
				t.Fatal("indeterminate ownership must not patch the Pod")
			}
			// Once the sibling is settled, here as another Pod's record, a
			// repeated DEL finishes the withdrawal from the preserved record.
			other := &localtypes.NetConf{}
			other.PodUID = "another-pod"
			if err := utils.SaveNetConf("sandbox-b", config.DefaultCNIDir, "ib1", other); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := delNetwork(t.Context(), args, standalone(withdrawer(f))); err != nil {
					t.Fatal(err)
				}
			}
			if connectiondetails.IsCurrent(f.pod.Annotations[connectiondetails.Annotation], mine) {
				t.Fatal("repeated DEL left its publication after ownership became readable")
			}
			if _, err := readRecordedPublication(args); !errors.Is(err, errNoRecord) {
				t.Fatalf("completed withdrawal left its ownership record: %v", err)
			}
			if f.pod.Annotations[connectiondetails.StatusAnnotation] != status {
				t.Fatal("DEL changed retained readiness status")
			}
		})
	}
}

func TestDELDoesNotIgnoreCommittedRecordContainingTempMarker(t *testing.T) {
	f, args, mine := withdrawalCacheTest(t)
	later := *mustRecord(t, args)
	later.PublishedAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := utils.SaveNetConf("sandbox.tmp-123", config.DefaultCNIDir, "ib1", later); err != nil {
		t.Fatal(err)
	}
	patches := f.patches
	if err := delNetwork(t.Context(), args, standalone(withdrawer(f))); err != nil {
		t.Fatal(err)
	}
	checkEntry(t, f, mine, 1)
	if f.patches != patches {
		t.Fatal("DEL ignored the newer sandbox's committed ownership record")
	}
}
