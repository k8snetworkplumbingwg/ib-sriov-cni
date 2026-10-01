package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// unacknowledgedPatch applies the PATCH and then reports a transport failure:
// the API server committed the write and the response was lost. afterApply
// runs once the write has landed, so a test can also take the API away.
type unacknowledgedPatch struct {
	*annotationClientFake
	afterApply func()
}

func (c *unacknowledgedPatch) PatchAnnotation(ctx context.Context, namespace, name, value, rv string, present bool) error {
	if err := c.annotationClientFake.PatchAnnotation(ctx, namespace, name, value, rv, present); err != nil {
		return err
	}
	if c.afterApply != nil {
		c.afterApply()
	}
	return io.ErrUnexpectedEOF
}

func publishingConf(t *testing.T, args *skel.CmdArgs) *localtypes.NetConf {
	t.Helper()
	conf, err := config.LoadConf(args.StdinData)
	if err != nil {
		t.Fatal(err)
	}
	return conf
}

func detailsOf(t *testing.T, f *annotationClientFake) map[string]connectiondetails.Detail {
	t.Helper()
	details, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	return details
}

// A publication write that the API server never answered, because the
// request was canceled or its budget expired, is a failed publication. It
// must not read as success: client-go's retry helper reports the last
// retriable error for an interrupted loop, which is nil when the interrupted
// attempt was the first, and a retained Ready would then satisfy the wait for
// an attachment that was never published.
func TestPublicationCancellationIsNotSuccess(t *testing.T) {
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(terminal.Error(), func(t *testing.T) {
			cacheDirAt(t)
			api, _ := newAnnotationTest(t)
			args := sandboxArgs("unpublished-sandbox")
			conf := publishingConf(t, args)
			mine := annotationEntry("net-a", testPF, 0)
			api.pod.Annotations[connectiondetails.StatusAnnotation] = `{"` + mine.Key() + `":{"Status":"Ready"}}`
			api.patchHook = func() error { return terminal }
			_, err := publishRecorded(t.Context(), api, args, conf,
				func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
			if !errors.Is(err, terminal) {
				t.Fatalf("publication after a PATCH that returned %v: err=%v; want that error", terminal, err)
			}
			if len(detailsOf(t, api)) != 0 {
				t.Fatalf("details were written: %v", api.pod.Annotations[connectiondetails.Annotation])
			}
			if _, recorded := recordedPublication(args); recorded {
				t.Fatal("a publication the API server rejected nothing of, and that did not land, left a record")
			}
		})
	}
}

// A withdrawal write the API server never answered is a failed DEL: the
// error reaches the runtime, which retries the DEL, and the record that
// authorizes the withdrawal stays for that retry.
func TestWithdrawalCancellationKeepsOwnershipForRetry(t *testing.T) {
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(terminal.Error(), func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			api, _ := newAnnotationTest(t)
			args := sandboxArgs("withdrawal-sandbox")
			args.Netns = ""
			mine := annotationEntry("net-a", testPF, 0)
			publishFrom(t, api, args, mine)
			api.patchHook = func() error { return terminal }
			err := delNetwork(t.Context(), args, standalone(withdrawer(api)))
			_, recorded := recordedPublication(args)
			if !errors.Is(err, terminal) || !recorded {
				t.Fatalf("DEL after a PATCH that returned %v: err=%v record=%v; want the error and the record kept", terminal, err, recorded)
			}
			checkEntry(t, api, mine, 1)
		})
	}
}

// A write whose outcome the API server did not report may have been applied.
// The authoritative copy decides: a Pod that carries the member confirms the
// publication, which then stands; one that does not means nothing was written
// and the record is undone, so a sandbox that never published owns nothing; a
// read that fails keeps the record, so this sandbox's rollback DEL can still
// withdraw a write that may have landed. A rejection the server reported is
// definite and undoes the record without a read.
func TestUnacknowledgedPublicationWriteOutcome(t *testing.T) {
	mine := annotationEntry("net-a", testPF, 0)
	t.Run("landed: confirmed and owned", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		args := sandboxArgs("confirmed-sandbox")
		args.Netns = ""
		client := &unacknowledgedPatch{annotationClientFake: api}
		if _, err := publishRecorded(t.Context(), client, args, publishingConf(t, args),
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
			t.Fatalf("a write the authoritative copy confirms failed the publication: %v", err)
		}
		checkEntry(t, api, mine, 1)
		mustRecord(t, args)
		if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", args.ContainerID, err)
		}
		if len(detailsOf(t, api)) != 0 {
			t.Fatal("the rollback DEL did not withdraw the confirmed publication")
		}
	})
	t.Run("not landed: record undone, earlier owner kept", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		earlier, later := sandboxArgs("earlier-sandbox"), sandboxArgs("later-sandbox")
		later.Netns = ""
		published := annotationEntry("net-a", testPF, 1)
		publishFrom(t, api, earlier, published)
		time.Sleep(time.Millisecond)
		api.patchHook = func() error { return io.ErrUnexpectedEOF }
		_, err := publishRecorded(t.Context(), api, later, publishingConf(t, later),
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err=%v; want the lost-response error", err)
		}
		api.patchHook = nil
		if _, recorded := recordedPublication(later); recorded {
			t.Fatal("a write the authoritative copy shows did not land left a record")
		}
		if err := delNetwork(t.Context(), later, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", later.ContainerID, err)
		}
		checkEntry(t, api, published, 1)
		mustRecord(t, earlier)
	})
	t.Run("unknown: record kept, rollback withdraws", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		args := sandboxArgs("unknown-sandbox")
		args.Netns = ""
		apiDown := errors.New("api unreachable")
		client := &unacknowledgedPatch{annotationClientFake: api, afterApply: func() { api.getErr = apiDown }}
		_, err := publishRecorded(t.Context(), client, args, publishingConf(t, args),
			func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err=%v; want the lost-response error when the confirming read fails too", err)
		}
		mustRecord(t, args)
		api.getErr = nil
		if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", args.ContainerID, err)
		}
		if len(detailsOf(t, api)) != 0 {
			t.Fatal("the rollback DEL did not withdraw a write that had landed")
		}
	})
}

// conflictThenUnacknowledged rejects the first PATCH as a stale
// resourceVersion, then applies the retry and loses its response. afterCommit
// runs once the retry has landed, so a test can also take the API away.
type conflictThenUnacknowledged struct {
	*annotationClientFake
	terminal    error
	afterCommit func()
	writes      int
}

func (c *conflictThenUnacknowledged) PatchAnnotation(ctx context.Context, namespace, name, value, rv string, present bool) error {
	c.writes++
	if c.writes == 1 {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, name, errors.New("resourceVersion changed"))
	}
	if err := c.annotationClientFake.PatchAnnotation(ctx, namespace, name, value, rv, present); err != nil {
		return err
	}
	if c.afterCommit != nil {
		c.afterCommit()
	}
	return c.terminal
}

// A conflict before the interrupted attempt must not hide that attempt's
// outcome: client-go reports the earlier conflict for an interrupted loop, and
// a conflict is a definite rejection, which would undo the record of a write
// that landed. The final attempt decides: confirmed when the authoritative
// copy carries the member, kept as unconfirmed intent when that read fails.
func TestConflictThenInterruptedWriteSettlesByTheFinalOutcome(t *testing.T) {
	mine := annotationEntry("net-a", testPF, 0)
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(terminal.Error()+", confirmable", func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			api, _ := newAnnotationTest(t)
			args := sandboxArgs("conflict-then-committed")
			args.Netns = ""
			client := &conflictThenUnacknowledged{annotationClientFake: api, terminal: terminal}
			if _, err := publishRecorded(t.Context(), client, args, publishingConf(t, args),
				func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil }); err != nil {
				t.Fatalf("a landed write the authoritative copy confirms failed the publication: %v", err)
			}
			mustRecord(t, args)
			if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err != nil {
				t.Fatalf("rollback DEL of %s: %v", args.ContainerID, err)
			}
			if len(detailsOf(t, api)) != 0 {
				t.Fatal("the rollback DEL stranded the committed publication")
			}
		})
		t.Run(terminal.Error()+", API down after the write", func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			api, _ := newAnnotationTest(t)
			args := sandboxArgs("conflict-then-committed-unconfirmed")
			args.Netns = ""
			apiDown := errors.New("api unreachable")
			client := &conflictThenUnacknowledged{annotationClientFake: api, terminal: terminal,
				afterCommit: func() { api.getErr = apiDown }}
			_, err := publishRecorded(t.Context(), client, args, publishingConf(t, args),
				func(string, string) (int, string, error) { return mine.DeviceIndex, mine.PfGUID, nil })
			if !errors.Is(err, terminal) || apierrors.IsConflict(err) {
				t.Fatalf("err=%v; want the final attempt's %v, not the earlier conflict", err, terminal)
			}
			mustRecord(t, args)
			api.getErr = nil
			if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err != nil {
				t.Fatalf("rollback DEL of %s: %v", args.ContainerID, err)
			}
			if len(detailsOf(t, api)) != 0 {
				t.Fatal("the rollback DEL stranded the committed publication")
			}
		})
	}
}

// A record whose write was neither acknowledged nor confirmed is intent, not
// ownership. A DEL settles it against the authoritative copy before it
// withdraws anything: a member that is the one this sandbox tried to publish
// is withdrawn; a member that differs belongs to another sandbox and is left,
// and the intent is dropped; an authoritative copy that cannot be read
// withdraws nothing and keeps the intent for the next DEL. A later confirmed
// publication of the same function by another sandbox takes precedence.
func TestUnconfirmedPublicationIsIntentNotOwnership(t *testing.T) {
	attempted := annotationEntry("net-a", testPF, 0)
	unconfirmedPublish := func(t *testing.T, api *annotationClientFake, args *skel.CmdArgs, landed bool) {
		t.Helper()
		apiDown := errors.New("api unavailable after the transport failure")
		var client podAnnotationClient = api
		if landed {
			client = &unacknowledgedPatch{annotationClientFake: api, afterApply: func() { api.getErr = apiDown }}
		} else {
			api.patchHook = func() error { api.getErr = apiDown; return io.ErrUnexpectedEOF }
		}
		_, err := publishRecorded(t.Context(), client, args, publishingConf(t, args),
			func(string, string) (int, string, error) { return attempted.DeviceIndex, attempted.PfGUID, nil })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err=%v; want the lost-response error", err)
		}
		api.patchHook = nil
		api.getErr = nil
		if own := mustRecord(t, args); !own.PublicationUnconfirmed {
			t.Fatal("an unacknowledged, unconfirmed write left a record that claims ownership")
		}
	}
	t.Run("not landed: another sandbox's member and a sibling are kept", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		earlier, later := sandboxArgs("earlier-owner"), sandboxArgs("unconfirmed-sandbox")
		later.Netns = ""
		old := annotationEntry("net-a", testPF, 1)
		publishFrom(t, api, earlier, old)
		sibling := annotationEntry("net-b", "0002:03:00.0", 2)
		api.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{old.Key(): old, sibling.Key(): sibling})
		time.Sleep(time.Millisecond)
		unconfirmedPublish(t, api, later, false)
		if err := delNetwork(t.Context(), later, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", later.ContainerID, err)
		}
		checkEntry(t, api, old, 2)
		checkEntry(t, api, sibling, 2)
		if _, recorded := recordedPublication(later); recorded {
			t.Fatal("intent that proved not to be ownership was kept after DEL")
		}
		mustRecord(t, earlier)
	})
	t.Run("landed: DEL withdraws the member it finds to be its own", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		args := sandboxArgs("unconfirmed-landed")
		args.Netns = ""
		unconfirmedPublish(t, api, args, true)
		checkEntry(t, api, attempted, 1)
		if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", args.ContainerID, err)
		}
		if len(detailsOf(t, api)) != 0 {
			t.Fatal("the landed publication was not withdrawn")
		}
	})
	t.Run("DEL cannot read: nothing withdrawn, intent kept", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		args := sandboxArgs("unconfirmed-unreadable")
		args.Netns = ""
		unconfirmedPublish(t, api, args, true)
		api.getErr = errors.New("api still unavailable")
		if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err == nil {
			t.Fatal("DEL reported success without settling the unconfirmed intent")
		}
		checkEntry(t, api, attempted, 1)
		mustRecord(t, args)
	})
	t.Run("a later confirmed publication takes precedence", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		first, second := sandboxArgs("unconfirmed-first"), sandboxArgs("confirmed-second")
		first.Netns = ""
		unconfirmedPublish(t, api, first, false)
		time.Sleep(time.Millisecond)
		publishFrom(t, api, second, attempted)
		if err := delNetwork(t.Context(), first, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", first.ContainerID, err)
		}
		checkEntry(t, api, attempted, 1)
		mustRecord(t, second)
	})
}

// An unconfirmed record belongs to a sandbox whose ADD failed; it is intent,
// so it must not stop the sandbox that did publish from withdrawing. The
// earlier sandbox withdraws only a member equal to the one it published:
// when the later write never landed that member is still its own and goes;
// when the later write did land, the member differs, the earlier sandbox
// leaves it, and the later sandbox's own DEL withdraws it.
func TestEarlierOwnerWithdrawsOverAnUnconfirmedSuccessor(t *testing.T) {
	old := annotationEntry("net-a", testPF, 1)
	attempted := annotationEntry("net-a", testPF, 0)
	apiDown := errors.New("api unavailable after the transport failure")
	t.Run("successor did not land: the earlier member is withdrawn", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		earlier, later := sandboxArgs("confirmed-earlier"), sandboxArgs("unconfirmed-later")
		earlier.Netns, later.Netns = "", ""
		publishFrom(t, api, earlier, old)
		time.Sleep(time.Millisecond)
		api.patchHook = func() error { api.getErr = apiDown; return io.ErrUnexpectedEOF }
		if _, err := publishRecorded(t.Context(), api, later, publishingConf(t, later),
			func(string, string) (int, string, error) { return attempted.DeviceIndex, attempted.PfGUID, nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err=%v; want the lost-response error", err)
		}
		api.patchHook, api.getErr = nil, nil
		if err := delNetwork(t.Context(), earlier, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", earlier.ContainerID, err)
		}
		if len(detailsOf(t, api)) != 0 {
			t.Fatalf("the earlier owner left its member behind an unconfirmed successor: %v", detailsOf(t, api))
		}
		if err := delNetwork(t.Context(), later, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", later.ContainerID, err)
		}
		for _, args := range []*skel.CmdArgs{earlier, later} {
			if _, recorded := recordedPublication(args); recorded {
				t.Fatalf("record of %s survived its DEL", args.ContainerID)
			}
		}
	})
	t.Run("successor landed: the earlier sandbox leaves it, the successor withdraws it", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		earlier, later := sandboxArgs("confirmed-earlier"), sandboxArgs("unconfirmed-later")
		earlier.Netns, later.Netns = "", ""
		publishFrom(t, api, earlier, old)
		time.Sleep(time.Millisecond)
		client := &unacknowledgedPatch{annotationClientFake: api, afterApply: func() { api.getErr = apiDown }}
		if _, err := publishRecorded(t.Context(), client, later, publishingConf(t, later),
			func(string, string) (int, string, error) { return attempted.DeviceIndex, attempted.PfGUID, nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err=%v; want the lost-response error", err)
		}
		api.getErr = nil
		if err := delNetwork(t.Context(), earlier, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", earlier.ContainerID, err)
		}
		checkEntry(t, api, attempted, 1)
		if err := delNetwork(t.Context(), later, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", later.ContainerID, err)
		}
		if len(detailsOf(t, api)) != 0 {
			t.Fatalf("the successor did not withdraw its landed member: %v", detailsOf(t, api))
		}
	})
}

// unlandedUnknown is a publication whose PATCH returns a lost response without
// applying anything, while the confirming read is unavailable: the record is
// left as unconfirmed intent.
func unlandedUnknown(t *testing.T, api *annotationClientFake, args *skel.CmdArgs, detail connectiondetails.Detail) {
	t.Helper()
	api.patchHook = func() error {
		api.getErr = errors.New("confirming read unavailable")
		return io.ErrUnexpectedEOF
	}
	_, err := publishRecorded(t.Context(), api, args, publishingConf(t, args),
		func(string, string) (int, string, error) { return detail.DeviceIndex, detail.PfGUID, nil })
	api.patchHook, api.getErr = nil, nil
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("publication err=%v, want the lost-response error", err)
	}
	if !mustRecord(t, args).PublicationUnconfirmed {
		t.Fatal("the record is not unconfirmed intent")
	}
}

// Ownership is decided from every later record of the function, not from the
// first file the directory lists. With an unconfirmed intent that sorts before
// a confirmed successor, the earlier owner's DEL must still see the confirmed
// successor and withdraw nothing.
func TestPendingSuccessorDoesNotHideAConfirmedOne(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	earlier, pending, current := sandboxArgs("00-earlier"), sandboxArgs("10-pending"), sandboxArgs("20-current")
	earlier.Netns = ""
	detail := annotationEntry("net-a", testPF, 0)
	publishFrom(t, api, earlier, detail)
	time.Sleep(time.Millisecond)
	unlandedUnknown(t, api, pending, annotationEntry("net-a", testPF, 1))
	time.Sleep(time.Millisecond)
	publishFrom(t, api, current, detail)
	if err := delNetwork(t.Context(), earlier, standalone(withdrawer(api))); err != nil {
		t.Fatalf("rollback DEL of %s: %v", earlier.ContainerID, err)
	}
	checkEntry(t, api, detail, 1)
	mustRecord(t, current)
}

// The record is intent until the write is acknowledged or confirmed. A process
// that dies at the write boundary leaves intent behind, never a claim of
// ownership, so its DEL settles against the authoritative copy and leaves a
// member it did not publish. Checked in-process at the PATCH boundary and with
// a real child process that exits there.
func TestIntentIsDurableBeforeTheWrite(t *testing.T) {
	const deathEnv = "IB_SRIOV_CNI_TEST_DEATH_AT_PATCH"
	if cache := os.Getenv(deathEnv); cache != "" {
		config.DefaultCNIDir, config.CniFileLockDir = cache, os.Getenv(deathEnv+"_LOCKS")
		api, _ := newAnnotationTest(t)
		previous := annotationEntry("net-a", testPF, 1)
		api.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{previous.Key(): previous})
		args := sandboxArgs("terminated-new")
		api.patchHook = func() error { os.Exit(87); return nil }
		_, _ = publishRecorded(t.Context(), api, args, publishingConf(t, args),
			func(string, string) (int, string, error) { return 0, previous.PfGUID, nil })
		t.Fatal("the child did not exit at the PATCH boundary")
	}
	t.Run("the record on disk is intent while the write is in flight", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		args := sandboxArgs("in-flight")
		var duringWrite *localtypes.NetConf
		api.patchHook = func() error { duringWrite = mustRecord(t, args); return nil }
		attempted := annotationEntry("net-a", testPF, 0)
		if _, err := publishRecorded(t.Context(), api, args, publishingConf(t, args),
			func(string, string) (int, string, error) { return attempted.DeviceIndex, attempted.PfGUID, nil }); err != nil {
			t.Fatal(err)
		}
		if duringWrite == nil || !duringWrite.PublicationUnconfirmed {
			t.Fatalf("record during the write: %+v; want unconfirmed intent", duringWrite)
		}
		if after := mustRecord(t, args); after.PublicationUnconfirmed {
			t.Fatal("the record was not promoted after the acknowledged write")
		}
	})
	t.Run("a process that dies at the write grants itself nothing", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		api, _ := newAnnotationTest(t)
		earlier, later := sandboxArgs("old-owner"), sandboxArgs("terminated-new")
		later.Netns = ""
		previous := annotationEntry("net-a", testPF, 1)
		publishFrom(t, api, earlier, previous)
		child := exec.Command(os.Args[0], "-test.run=^TestIntentIsDurableBeforeTheWrite$")
		child.Env = append(os.Environ(), deathEnv+"="+config.DefaultCNIDir, deathEnv+"_LOCKS="+config.CniFileLockDir)
		output, err := child.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 87 {
			t.Fatalf("child err=%v output=%s; want exit 87 at the PATCH boundary", err, output)
		}
		if !mustRecord(t, later).PublicationUnconfirmed {
			t.Fatal("the dead process left a record that claims ownership")
		}
		if err := delNetwork(t.Context(), later, standalone(withdrawer(api))); err != nil {
			t.Fatalf("rollback DEL of %s: %v", later.ContainerID, err)
		}
		checkEntry(t, api, previous, 1)
	})
}

// A sandbox whose DEL failed during an API outage is retried with the same
// container ID. If the retried ADD's write is left unknown and did not land,
// the live member is this sandbox's own earlier confirmed publication: the
// record keeps that prior member, and DEL withdraws it.
func TestUnknownRetryKeepsTheSandboxsEarlierMember(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	api, _ := newAnnotationTest(t)
	args := sandboxArgs("same-sandbox")
	args.Netns = ""
	previous := annotationEntry("net-a", testPF, 1)
	publishFrom(t, api, args, previous)
	api.getErr = errors.New("API unavailable during the intervening DEL")
	if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err == nil {
		t.Fatal("the intervening DEL succeeded without the API")
	}
	mustRecord(t, args)
	api.getErr = nil
	time.Sleep(time.Millisecond)
	unlandedUnknown(t, api, args, annotationEntry("net-a", testPF, 0))
	checkEntry(t, api, previous, 1)
	if err := delNetwork(t.Context(), args, standalone(withdrawer(api))); err != nil {
		t.Fatalf("rollback DEL of %s: %v", args.ContainerID, err)
	}
	if details := detailsOf(t, api); len(details) != 0 {
		t.Fatalf("the retry forgot this sandbox's earlier member: %+v", details)
	}
}
