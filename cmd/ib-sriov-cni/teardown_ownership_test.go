package main

// Teardown ownership: which sandbox may withdraw a published attachment, and
// what a DEL does when a Pod is terminating, a record is unreadable, or a later
// sandbox has republished.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/gofrs/flock"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

func TestDELConfirmsUIDBeforeAcceptingCachedTermination(t *testing.T) {
	old := cachedPod()
	now := metav1.Now()
	old.DeletionTimestamp = &now
	pods, _ := startNodeCache(t, old)
	api, args := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	api.pod.UID = "uid-2"
	api.pod.ResourceVersion = "2"
	api.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t,
		map[string]connectiondetails.Detail{mine.Key(): mine})
	args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1;K8S_POD_UID=uid-2"
	writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}
	if err := removePodConnectionDetails(t.Context(), writer, args, mine.DeviceID, ""); err != nil {
		t.Fatal(err)
	}
	if connectiondetails.IsCurrent(api.pod.Annotations[connectiondetails.Annotation], mine) {
		t.Fatalf("live uid-2 entry stranded: DEL accepted cached terminating uid-1; live GETs=%d PATCHes=%d", api.getCalls, api.patches)
	}
	if api.getCalls != 1 || api.patches != 1 {
		t.Fatalf("GETs=%d PATCHes=%d, want one authoritative UID confirmation and one withdrawal", api.getCalls, api.patches)
	}
}

func TestDELTerminatingRawAnnotationsPreserved(t *testing.T) {
	for _, finalizer := range []bool{false, true} {
		for _, details := range []string{"", `{"broken"`, `{"legacy":"raw"}`} {
			f, args := newAnnotationTest(t)
			now := metav1.Now()
			f.pod.DeletionTimestamp = &now
			if finalizer {
				f.pod.Finalizers = []string{"controller.example.test/cleanup"}
			}
			f.pod.Annotations[connectiondetails.Annotation] = details
			f.pod.Annotations[connectiondetails.StatusAnnotation] = `{"key":{"Status":"Ready"}}`
			before := f.pod.DeepCopy()
			for range 2 {
				if err := removePodConnectionDetails(t.Context(), f, args, "0000:03:00.0", ""); err != nil {
					t.Fatal(err)
				}
			}
			if f.patches != 0 || !reflect.DeepEqual(before, f.pod) {
				t.Fatalf("terminating Pod changed: patches=%d finalizer=%v details=%q", f.patches, finalizer, details)
			}
		}
	}
}

func TestDELDeletionRaceConflictAndInvalid(t *testing.T) {
	for name, rejection := range map[string]error{
		"Conflict": apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("stale")),
		"Invalid":  apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "vm-1", nil),
	} {
		t.Run(name, func(t *testing.T) {
			mine := annotationEntry("net-a", "0000:03:00.0", 0)
			_, args := newAnnotationTest(t)
			pods, _ := startNodeCache(t, divergentPod(t, "1", map[string]connectiondetails.Detail{mine.Key(): mine}))
			live := divergentPod(t, "2", map[string]connectiondetails.Detail{mine.Key(): mine})
			now := metav1.Now()
			live.DeletionTimestamp = &now
			before := live.DeepCopy()
			api := &rejectedPatchAPI{fencedAPI: &fencedAPI{pod: live}, rejection: rejection}
			writer := &nodePodWriter{reader: nodePodReader{pods: pods, api: api}}
			if err := removePodConnectionDetails(t.Context(), writer, args, mine.DeviceID, ""); err != nil {
				t.Fatal(err)
			}
			if api.patches != 1 || api.reads != 1 || api.mutations != 0 || !reflect.DeepEqual(before, api.pod) {
				t.Fatalf("patches=%d liveReads=%d mutations=%d, want 1/1/0 with Pod unchanged", api.patches, api.reads, api.mutations)
			}
		})
	}
}

func TestTerminatingDELStillCleansOwnershipCache(t *testing.T) {
	for _, daemonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "daemon"}[daemonMode], func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			f, args := newAnnotationTest(t)
			mine := annotationEntry("net-a", "0000:03:00.0", 0)
			f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{mine.Key(): mine})
			now := metav1.Now()
			f.pod.DeletionTimestamp = &now
			args.ContainerID = "terminating-sandbox"
			args.Netns = filepath.Join(t.TempDir(), "gone-netns")
			args.StdinData = []byte(`{"name":"net-a","type":"ib-sriov","cniVersion":"0.3.1","deviceID":"0000:03:00.0","ibKubernetesEnabled":true,"publishPfGuid":true,"vfioPciMode":true}`)
			conf, err := config.LoadConf(args.StdinData)
			if err != nil {
				t.Fatal(err)
			}
			conf.PodUID = "uid-1"
			if err := utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, conf); err != nil {
				t.Fatal(err)
			}
			withdraw := func(a *skel.CmdArgs, n *localtypes.NetConf) error {
				return removePodConnectionDetailsExpecting(t.Context(), f, a, n.DeviceID, n.PodUID, expectedMembers(n))
			}
			inv := standalone(withdraw)
			if daemonMode {
				pods, _ := startNodeCache(t, f.pod)
				inv = invocation{kube: daemonWithdrawer(t.Context(), nodePodReader{pods: pods, api: f})}
			}
			if err := delNetwork(t.Context(), args, inv); err != nil {
				t.Fatal(err)
			}
			if entries, err := os.ReadDir(config.DefaultCNIDir); err != nil || len(entries) != 0 {
				t.Fatalf("ownership cache not cleaned: entries=%v err=%v", entries, err)
			}
			if f.patches != 0 || !connectiondetails.IsCurrent(f.pod.Annotations[connectiondetails.Annotation], mine) {
				t.Fatalf("details not preserved: patches=%d", f.patches)
			}
		})
	}
}

type rejectedPatchAPI struct {
	*fencedAPI
	rejection error
	mutations int
}

func (f *rejectedPatchAPI) PatchAnnotation(ctx context.Context, namespace, name, value, rv string, present bool) error {
	if rv != f.pod.ResourceVersion {
		f.patches++
		return f.rejection
	}
	f.mutations++
	return f.fencedAPI.PatchAnnotation(ctx, namespace, name, value, rv, present)
}

func TestRepeatedDELDoesNotRemoveNewSandbox(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	earlier, later := sandboxArgs("sandbox-a"), sandboxArgs("sandbox-b")
	publishFrom(t, f, earlier, mine)
	time.Sleep(time.Millisecond)
	publishFrom(t, f, later, mine)
	for range 2 {
		if err := delNetwork(t.Context(), earlier, standalone(withdrawer(f))); err != nil {
			t.Fatal(err)
		}
	}
	checkEntry(t, f, mine, 1)
	if _, ok := recordedPublication(earlier); ok {
		t.Fatal("old cache file survives teardown")
	}
	if _, ok := recordedPublication(later); !ok {
		t.Fatal("later cache file was removed")
	}
}

func TestUnrelatedPodOrDeviceDoesNotSuppressOwnWithdrawal(t *testing.T) {
	for _, change := range []string{"UID", "device"} {
		t.Run(change, func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			f, _ := newAnnotationTest(t)
			mine := annotationEntry("net-a", "0000:03:00.0", 0)
			own := sandboxArgs("sandbox-a")
			publishFrom(t, f, own, mine)
			other := &localtypes.NetConf{}
			other.PodUID, other.DeviceID = "uid-1", mine.DeviceID
			other.PublishedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
			if change == "UID" {
				other.PodUID = "uid-other"
			} else {
				other.DeviceID = "0002:03:00.0"
			}
			if err := utils.SaveNetConf("other", config.DefaultCNIDir, "ib1", other); err != nil {
				t.Fatal(err)
			}
			if _, err := withdrawConnectionDetails(t.Context(), own, withdrawer(f)); err != nil {
				t.Fatal(err)
			}
			if connectiondetails.IsCurrent(f.pod.Annotations[connectiondetails.Annotation], mine) {
				t.Fatal("unrelated record suppressed own withdrawal")
			}
		})
	}
}

func TestReadyDoesNotOverrideObservedDeletion(t *testing.T) {
	for _, uid := range []string{"uid-1", "other"} {
		f, _ := newAnnotationTest(t)
		f.pod.UID = "uid-1"
		now := metav1.Now()
		f.pod.DeletionTimestamp = &now
		f.pod.Annotations[connectiondetails.StatusAnnotation] = `{"ns1/net-a/0000:03:00.0":{"Status":"Ready"}}`
		err := connectiondetails.WaitForReady(context.Background(), f, "ns1", "vm-1", uid, "ns1/net-a/0000:03:00.0")
		want := "being deleted"
		if uid != "uid-1" {
			want = "was replaced"
		}
		if err == nil || !strings.Contains(err.Error(), want) || f.getCalls != 1 {
			t.Fatalf("uid=%s err=%v reads=%d, want %s on first read", uid, err, f.getCalls, want)
		}
	}
}

// An unreadable own record is indeterminate, not proof that nothing was
// published: DEL withdraws nothing, keeps the file for a later look, and still
// succeeds, whatever UID the runtime passes.
func TestOwnRecordParseErrorWithdrawsNothingAndKeepsTheFile(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	args := sandboxArgs("sandbox-a")
	publishFrom(t, f, args, mine)
	if err := os.WriteFile(filepath.Join(config.DefaultCNIDir, "sandbox-a-ib1"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	patches := f.patches
	if err := delNetwork(t.Context(), args, standalone(withdrawer(f))); err != nil {
		t.Fatal(err)
	}
	if f.patches != patches || !connectiondetails.IsCurrent(f.pod.Annotations[connectiondetails.Annotation], mine) {
		t.Fatal("an indeterminate own record must not authorize a withdrawal")
	}
	if _, err := os.Stat(filepath.Join(config.DefaultCNIDir, "sandbox-a-ib1")); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitInterfaceWinsOverConflictingConfigName(t *testing.T) {
	ns, name, err := connectiondetails.ResolveNetwork("ns1",
		`[{"name":"net-a","interface":"ib1"},{"name":"net-b"}]`, "ib1", "net-b")
	if err != nil || ns != "ns1" || name != "net-a" {
		t.Fatalf("explicit identity was overridden: %q/%q err=%v", ns, name, err)
	}
}

func TestImplicitRetryUsesTheSameResolverForMergeAndNoop(t *testing.T) {
	f, args := newAnnotationTest(t)
	f.pod.Annotations[testNetworksAnnotation] = "net-a,net-a"
	args.IfName = "net2"
	args.StdinData = []byte(`{"name":"net-a"}`)
	for range 2 {
		pub, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0",
			func(string, string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil })
		if err != nil || pub.Key != "ns1/net-a/0000:03:00.0" {
			t.Fatalf("publication=%+v err=%v", pub, err)
		}
	}
	if f.patches != 1 {
		t.Fatalf("unchanged retry wrote %d times, want one initial write", f.patches)
	}
}

func TestDELFailureBoundaries(t *testing.T) {
	for _, failure := range []string{"cache write fails after publication", "rollback has empty netns"} {
		t.Run(failure, func(t *testing.T) {
			cacheDirAt(t)
			vfioPFAt(t)
			pods, client := startNodeCache(t, concurrencyPod("vm-a", "ib1"))
			reader := nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(client)}
			device := func(string, string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil }
			args := &skel.CmdArgs{
				ContainerID: "fresh-sandbox", IfName: "ib1", Netns: filepath.Join(t.TempDir(), "gone"),
				Args:      "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-a",
				StdinData: []byte(`{"name":"net-a","type":"ib-sriov","cniVersion":"0.3.1","deviceID":"0000:03:00.0","ibKubernetesEnabled":true,"publishPfGuid":true,"vfioPciMode":true,"readinessTimeoutSeconds":1}`),
			}
			expectedError := "context deadline exceeded"
			if failure == "cache write fails after publication" {
				blocked := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(blocked, []byte("unavailable cache"), 0o600); err != nil {
					t.Fatal(err)
				}
				config.DefaultCNIDir = blocked
				expectedError = "error recording the published attachment"
			}
			_, _, addErr := addNetwork(t.Context(), args, invocation{kube: daemonPublisher(t.Context(), reader, device)})
			if addErr == nil || !strings.Contains(addErr.Error(), expectedError) {
				t.Fatalf("wanted %q; got %v", expectedError, addErr)
			}
			if failure == "rollback has empty netns" {
				args.Netns = ""
			}
			if err := delNetwork(t.Context(), args, invocation{kube: daemonWithdrawer(t.Context(), reader)}); err != nil {
				t.Fatal(err)
			}
			live, err := client.CoreV1().Pods("ns1").Get(t.Context(), "vm-a", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			details, err := connectiondetails.Parse(live.Annotations[connectiondetails.Annotation])
			if err != nil {
				t.Fatal(err)
			}
			if len(details) != 0 {
				t.Errorf("DEL succeeded but left %d published member(s)", len(details))
			}
			if failure == "rollback has empty netns" {
				files, err := os.ReadDir(config.DefaultCNIDir)
				if err != nil {
					t.Fatal(err)
				}
				if len(files) != 0 {
					t.Errorf("DEL succeeded but left %d cached ownership file(s)", len(files))
				}
			}
		})
	}
}

// kubelet can run an earlier sandbox's DEL while a later sandbox's ADD is in
// flight. The ownership decision and the withdrawal it authorizes must be one
// step against publication: the node's ownership lock is held from the record
// read to the annotation write, so a concurrent ADD records and publishes only
// after this DEL has finished, and its record then stands.
func TestPublicationAfterFenceScanSurvivesOldDEL(t *testing.T) {
	cacheDirAt(t)
	vfioPFAt(t)
	f, _ := newAnnotationTest(t)
	mine := annotationEntry("net-a", "0000:03:00.0", 0)
	earlier, later := sandboxArgs("sandbox-a"), sandboxArgs("sandbox-b")
	publishFrom(t, f, earlier, mine)
	seamReached := false
	if _, err := withdrawConnectionDetails(t.Context(), earlier, func(args *skel.CmdArgs, conf *localtypes.NetConf) error {
		seamReached = true
		probe := flock.New(filepath.Join(config.CniFileLockDir, ownershipLockName))
		locked, err := probe.TryLock()
		if err != nil {
			t.Fatalf("probing the ownership lock: %v", err)
		}
		if locked {
			_ = probe.Unlock()
			t.Fatal("the ownership lock is free between the ownership decision and the withdrawal")
		}
		return withdrawer(f)(args, conf)
	}); err != nil {
		t.Fatal(err)
	}
	if !seamReached {
		t.Fatal("the withdrawal step was never reached")
	}
	// The later sandbox publishes after the lock is released and its record stands.
	publishFrom(t, f, later, mine)
	checkEntry(t, f, mine, 1)
	if superseded, _, err := laterPublication(later, mustRecord(t, later)); err != nil || superseded != "" {
		t.Fatalf("sanity: the later record must not read as superseded: %v %v", superseded, err)
	}
}

func mustRecord(t *testing.T, args *skel.CmdArgs) *localtypes.NetConf {
	t.Helper()
	own, ok := recordedPublication(args)
	if !ok {
		t.Fatalf("no record for %s", args.ContainerID)
	}
	return own
}

// A sandbox that records itself and then fails to publish holds no ownership.
// The record is undone, so its rollback DEL finds no record and withdraws
// nothing; otherwise the newest record of the attachment would belong to a
// sandbox that never published, and its DEL would take away the member the
// earlier sandbox published. A retry whose publication fails keeps the record
// of the publication that did happen. The earlier member differs in one field
// so that the merge has to write, and the write is refused.
func TestFailedPublicationLeavesNoOwnership(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("denied"))
	attempted := annotationEntry("net-a", testPF, 0)
	failingPublish := func(t *testing.T, f *annotationClientFake, args *skel.CmdArgs) {
		t.Helper()
		f.patchHook = func() error { return denied }
		netConf := &localtypes.NetConf{}
		netConf.DeviceID = attempted.DeviceID
		_, err := publishRecorded(t.Context(), f, args, netConf,
			func(string, string) (int, string, error) { return attempted.DeviceIndex, attempted.PfGUID, nil })
		if err == nil {
			t.Fatal("the refused publication reported success")
		}
		f.patchHook = nil
	}
	t.Run("replacement sandbox withdraws nothing", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		f, _ := newAnnotationTest(t)
		published := annotationEntry("net-a", testPF, 1)
		earlier, later := sandboxArgs("sandbox-a"), sandboxArgs("sandbox-b")
		publishFrom(t, f, earlier, published)
		time.Sleep(time.Millisecond)
		failingPublish(t, f, later)
		if _, ok := recordedPublication(later); ok {
			t.Fatal("a publication that failed left a record")
		}
		if err := delNetwork(t.Context(), later, standalone(withdrawer(f))); err != nil {
			t.Fatal(err)
		}
		checkEntry(t, f, published, 1)
		mustRecord(t, earlier)
	})
	t.Run("failed retry keeps the publication's record", func(t *testing.T) {
		cacheDirAt(t)
		vfioPFAt(t)
		f, _ := newAnnotationTest(t)
		published := annotationEntry("net-a", testPF, 1)
		own := sandboxArgs("sandbox-a")
		publishFrom(t, f, own, published)
		before := mustRecord(t, own)
		time.Sleep(time.Millisecond)
		failingPublish(t, f, own)
		after := mustRecord(t, own)
		if after.PublishedAt != before.PublishedAt || after.PodUID != before.PodUID {
			t.Fatalf("the refused retry changed the record from %s to %s", before.PublishedAt, after.PublishedAt)
		}
		if err := delNetwork(t.Context(), own, standalone(withdrawer(f))); err != nil {
			t.Fatal(err)
		}
		details, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
		if err != nil || len(details) != 0 {
			t.Fatalf("the sandbox that did publish could not withdraw its member: %v %v", details, err)
		}
	})
}
