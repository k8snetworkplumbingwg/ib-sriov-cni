package main

import (
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

// clockAt makes the publication clock read at until the test ends.
func clockAt(t *testing.T, at time.Time) {
	t.Helper()
	previous := publicationClock
	publicationClock = func() time.Time { return at }
	t.Cleanup(func() { publicationClock = previous })
}

// publishAtClock publishes and records the attachment as the given sandbox
// through the shared ADD path, with the wall clock reading at.
func publishAtClock(t *testing.T, api *annotationClientFake, args *skel.CmdArgs,
	detail connectiondetails.Detail, at time.Time,
) {
	t.Helper()
	clockAt(t, at)
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publishPodConnectionDetailsRecording(t.Context(), api, args, conf.DeviceID,
		func(string, string) (int, string, error) { return detail.DeviceIndex, detail.PfGUID, nil },
		&publicationRecord{args: args, netConf: conf})
	if err != nil {
		t.Fatal(err)
	}
	checkEntry(t, api, detail, 1)
}

// Publications on a node are sequential under the ownership lock, so the
// sandbox that published last is the owner whatever the wall clock read when
// each sandbox recorded. A clock that stands still or moves back between two
// sandboxes of one Pod must not let the earlier sandbox's late DEL take the
// later sandbox's live publication away.
func TestALaterSandboxOwnsThePublicationWhateverTheClockRead(t *testing.T) {
	base := time.Date(2026, time.September, 28, 12, 0, 0, 123456789, time.UTC)
	for name, laterClock := range map[string]time.Time{
		"clock moved forward": base.Add(time.Second),
		"clock stood still":   base,
		"clock moved back":    base.Add(-time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			api, _ := newAnnotationTest(t)
			detail := annotationEntry("net-a", "0000:03:00.0", 0)
			earlier, later := sandboxArgs("sandbox-a"), sandboxArgs("sandbox-b")
			earlier.Netns = filepath.Join(t.TempDir(), "gone-earlier-netns")
			later.Netns = filepath.Join(t.TempDir(), "gone-later-netns")

			publishAtClock(t, api, earlier, detail, base)
			publishAtClock(t, api, later, detail, laterClock)
			patchesBeforeDEL := api.patches

			if err := delNetwork(t.Context(), earlier, standalone(withdrawer(api))); err != nil {
				t.Fatalf("delNetwork(earlier): %v", err)
			}
			if !connectiondetails.IsCurrent(api.pod.Annotations[connectiondetails.Annotation], detail) {
				t.Errorf("the earlier sandbox's DEL withdrew the later sandbox's publication: patches=%d annotation=%s",
					api.patches-patchesBeforeDEL, api.pod.Annotations[connectiondetails.Annotation])
			}
			if api.patches != patchesBeforeDEL {
				t.Errorf("the earlier sandbox's DEL patched the Pod %d time(s), want none", api.patches-patchesBeforeDEL)
			}
			if _, ok := recordedPublication(later); !ok {
				t.Fatal("the later sandbox's record is gone")
			}
			if _, ok := recordedPublication(earlier); ok {
				t.Fatal("the earlier sandbox's DEL left its own record behind")
			}

			// Preserving the later sandbox's entry must not make it immortal:
			// its own DEL still withdraws it.
			if err := delNetwork(t.Context(), later, standalone(withdrawer(api))); err != nil {
				t.Fatal(err)
			}
			entries, err := connectiondetails.Parse(api.pod.Annotations[connectiondetails.Annotation])
			if err != nil || len(entries) != 0 {
				t.Fatalf("the later sandbox's own DEL did not withdraw: entries=%v err=%v", entries, err)
			}
		})
	}
}

// A record of the same attachment that carries no readable instant cannot be
// ordered against this sandbox's, so the DEL withdraws nothing and keeps its
// own record for a retry: taking a live publication away is the failure a
// withdrawal must never risk.
func TestDELWithdrawsNothingWhenAnotherRecordOfTheAttachmentCannotBeOrdered(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	detail := annotationEntry("net-a", "0000:03:00.0", 0)
	earlier := sandboxArgs("sandbox-a")
	earlier.Netns = filepath.Join(t.TempDir(), "gone-earlier-netns")
	publishAtClock(t, api, earlier, detail, time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC))

	unordered := &localtypes.NetConf{}
	unordered.DeviceID, unordered.PodUID, unordered.PublishedAt = detail.DeviceID, "uid-1", "not an instant"
	if err := utils.SaveNetConf("sandbox-b", config.DefaultCNIDir, "ib1", unordered); err != nil {
		t.Fatal(err)
	}
	patchesBeforeDEL := api.patches

	if err := delNetwork(t.Context(), earlier, standalone(withdrawer(api))); err != nil {
		t.Fatalf("delNetwork(earlier): %v", err)
	}
	if !connectiondetails.IsCurrent(api.pod.Annotations[connectiondetails.Annotation], detail) {
		t.Errorf("DEL withdrew a publication whose ownership it could not settle: annotation=%s",
			api.pod.Annotations[connectiondetails.Annotation])
	}
	if api.patches != patchesBeforeDEL {
		t.Errorf("DEL patched the Pod %d time(s), want none", api.patches-patchesBeforeDEL)
	}
	if _, ok := recordedPublication(earlier); !ok {
		t.Error("DEL dropped its own record although it withdrew nothing")
	}
}

// The instant a publication records follows the records already on the node:
// the wall clock when it is ahead of them, one nanosecond past the latest of
// them otherwise. Records that carry no readable instant do not take part.
func TestNextPublicationTimeNeverFallsBehindTheRecordsOnTheNode(t *testing.T) {
	cacheDirAt(t)
	now := time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)
	write := func(name, publishedAt string) {
		conf := &localtypes.NetConf{}
		conf.DeviceID, conf.PodUID, conf.PublishedAt = "0000:03:00.0", "uid-1", publishedAt
		if err := utils.SaveNetConf(name, config.DefaultCNIDir, "ib1", conf); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(config.DefaultCNIDir); err != nil {
		t.Fatal(err)
	}
	if got, err := nextPublicationTime(now); err != nil || !got.Equal(now) {
		t.Fatalf("no cache directory yet: got %v, %v; want the clock", got, err)
	}
	write("legacy", "")
	write("garbled", "not an instant")
	write("behind", now.Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	if got, err := nextPublicationTime(now); err != nil || !got.Equal(now) {
		t.Fatalf("clock ahead of every record: got %v, %v; want the clock", got, err)
	}
	ahead := now.Add(time.Minute)
	write("ahead", ahead.UTC().Format(time.RFC3339Nano))
	write("equal", now.UTC().Format(time.RFC3339Nano))
	if got, err := nextPublicationTime(now); err != nil || !got.Equal(ahead.Add(time.Nanosecond)) {
		t.Fatalf("a record ahead of the clock: got %v, %v; want %v", got, err, ahead.Add(time.Nanosecond))
	}
	if got, err := nextPublicationTime(ahead); err != nil || !got.Equal(ahead.Add(time.Nanosecond)) {
		t.Fatalf("a record equal to the clock: got %v, %v; want %v", got, err, ahead.Add(time.Nanosecond))
	}
}

// A record that cannot be read is passed over: DEL logs it and withdraws as
// usual, accepting that it might have been the later sandbox's. Otherwise one
// damaged file would stop every publishing DEL on the node from withdrawing
// until someone removed it.
func TestDELWithdrawsDespiteAnUnrelatedRecordThatCannotBeRead(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	detail := annotationEntry("net-a", "0000:03:00.0", 0)
	sandbox := sandboxArgs("sandbox-a")
	sandbox.Netns = filepath.Join(t.TempDir(), "gone-netns")
	publishAtClock(t, api, sandbox, detail, time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC))
	if err := os.WriteFile(filepath.Join(config.DefaultCNIDir, "damaged-sandbox-ib1"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	patchesBeforeDEL := api.patches

	if err := delNetwork(t.Context(), sandbox, standalone(withdrawer(api))); err != nil {
		t.Fatalf("delNetwork: %v", err)
	}
	entries, err := connectiondetails.Parse(api.pod.Annotations[connectiondetails.Annotation])
	if err != nil || len(entries) != 0 {
		t.Fatalf("DEL did not withdraw past an unreadable unrelated record: entries=%v err=%v", entries, err)
	}
	if api.patches != patchesBeforeDEL+1 {
		t.Fatalf("DEL patched the Pod %d time(s), want one withdrawal", api.patches-patchesBeforeDEL)
	}
	if _, ok := recordedPublication(sandbox); ok {
		t.Fatal("DEL left its own record behind after withdrawing")
	}
}
