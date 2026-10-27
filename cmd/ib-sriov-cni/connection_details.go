package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/gofrs/flock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/config"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/guidcache"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/utils"
)

// podAnnotationClient reads and writes one Pod's connection-details
// annotation. GetPod may answer from a cache; GetPodAuthoritative may not. The
// two are distinct because a cached copy cannot tell "nothing to do" from "the
// change has not reached me yet", and reporting the first when it is the second
// loses the operation silently.
type podAnnotationClient interface {
	GetPod(context.Context, string, string) (*corev1.Pod, error)
	GetPodAuthoritative(context.Context, string, string) (*corev1.Pod, error)
	PatchAnnotation(context.Context, string, string, string, string, bool) error
}

func publishConnectionDetails(ctx context.Context, args *skel.CmdArgs, netConf *localtypes.NetConf) error {
	client, err := connectiondetails.NewPodClient(netConf.Kubeconfig)
	if err != nil {
		return err
	}
	published, err := publishRecorded(ctx, client, args, netConf, deviceReader(ctx))
	if err != nil {
		return err
	}
	return awaitConnectionReady(ctx, client, args, netConf, published)
}

// ownershipLockName is the node-wide lock that makes "record ownership, then
// publish" and "decide ownership, then withdraw" each one step. kubelet can
// run an earlier sandbox's DEL while a later sandbox's ADD is in flight, and
// the two name the same key; without the lock the DEL could decide from a
// directory the ADD is about to change. It is separate from the device lock,
// which is held for seconds per rebind, and it is never held across the
// readiness wait.
const ownershipLockName = "ownership.lock"

func acquireOwnershipLock(ctx context.Context) (*flock.Flock, error) {
	return acquireObservedLock(ctx, config.CniFileLockDir, ownershipLockName, "ownership_lock")
}

// publishRecorded is the publishers' entry: identity and device first, then,
// under the ownership lock, this sandbox's record and the publication write.
// The record goes first so an ADD that cannot record fails before it publishes
// and its rollback has nothing to find; the write-then-record order left a
// published member with no record to authorize its withdrawal.
func publishRecorded(ctx context.Context, client podAnnotationClient, args *skel.CmdArgs,
	netConf *localtypes.NetConf, readDevice deviceResolver,
) (publication, error) {
	return publishPodConnectionDetailsRecording(ctx, client, args, netConf.DeviceID, readDevice,
		&publicationRecord{args: args, netConf: netConf})
}

// publicationClock is the wall clock a publication is recorded at; tests make
// it stand still or move back. The instant recorded never moves back either
// way: see nextPublicationTime.
var publicationClock = time.Now

// nextPublicationTime is the instant a publication is recorded at: the wall
// clock, unless a record on this node already carries that instant or a later
// one, in which case one nanosecond past the latest of them. DEL orders two
// sandboxes of one Pod by these instants, and publications on a node are
// sequential under the ownership lock, so the recorded order has to follow the
// publication order even when the wall clock stands still or moves back.
// A record without a readable instant carries no order and is passed over
// here; laterPublication refuses to order against it.
func nextPublicationTime(now time.Time) (time.Time, error) {
	entries, err := os.ReadDir(config.DefaultCNIDir)
	if errors.Is(err, os.ErrNotExist) {
		return now, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("list %s: %w", config.DefaultCNIDir, err)
	}
	next := now
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), utils.NetConfTempPrefix) {
			continue
		}
		record, err := readRecord(filepath.Join(config.DefaultCNIDir, entry.Name()))
		if err != nil {
			klog.ErrorS(err, "publication record passed over while ordering a new publication")
			continue
		}
		if at, err := publicationInstant(record); err == nil && !at.Before(next) {
			next = at.Add(time.Nanosecond)
		}
	}
	return next, nil
}

// recordPublication puts the ownership an ADD is about to establish on disk:
// the Pod UID it publishes to and the time, with the configuration DEL reads
// back. A first ADD that publishes and then times out is rolled back by a DEL
// that, on an older runtime, carries no K8S_POD_UID; without this record that
// DEL would have no evidence and the published member would outlive the
// sandbox. ADD writes the same file again when it completes.
func recordPublication(args *skel.CmdArgs, netConf *localtypes.NetConf, published publication, previous *localtypes.NetConf) error {
	at, err := nextPublicationTime(publicationClock())
	if err != nil {
		return fmt.Errorf("error recording the published attachment: %w", err)
	}
	netConf.PodUID = published.UID
	netConf.PublishedAt = at.UTC().Format(time.RFC3339Nano)
	netConf.PublishedPfGUID = published.Detail.PfGUID
	netConf.PublishedDeviceIndex = published.Detail.DeviceIndex
	// Intent until the write is acknowledged or confirmed: a process that
	// dies at the write leaves a record that claims nothing.
	netConf.PublicationUnconfirmed = true
	netConf.WithdrawExactMember = false
	// A retry of this sandbox may leave its previous confirmed member on the
	// Pod; that member stays this sandbox's to withdraw.
	netConf.PriorPublishedPfGUID, netConf.PriorPublishedDeviceIndex = "", 0
	if previous != nil {
		switch {
		case !previous.PublicationUnconfirmed && previous.PublishedPfGUID != "":
			netConf.PriorPublishedPfGUID, netConf.PriorPublishedDeviceIndex = previous.PublishedPfGUID, previous.PublishedDeviceIndex
		case previous.PriorPublishedPfGUID != "":
			netConf.PriorPublishedPfGUID, netConf.PriorPublishedDeviceIndex = previous.PriorPublishedPfGUID, previous.PriorPublishedDeviceIndex
		}
	}
	if err := utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, netConf); err != nil {
		return fmt.Errorf("error recording the published attachment: %w", err)
	}
	return nil
}

// publicationRecord is the record an ADD keeps of its publication, kept in the
// configuration the ADD saves again when it completes. It is written as
// intent before the publication write and promoted once the write is
// acknowledged or confirmed; promotion changes that same configuration, so
// the record the ADD completes with is the record the publication left.
type publicationRecord struct {
	args    *skel.CmdArgs
	netConf *localtypes.NetConf
}

func (r *publicationRecord) record(published publication, previous *localtypes.NetConf) error {
	return recordPublication(r.args, r.netConf, published, previous)
}

// promote marks the record a confirmed publication: the write was
// acknowledged, or the authoritative copy carries the member. The Pod now
// carries the member this sandbox published, so a member its previous
// publication left there is superseded and forgotten; a later write of that
// member belongs to the sandbox that writes it. A promotion that cannot be
// saved leaves the record as intent, in memory and on disk alike, which is the
// safe side: its DEL then withdraws only the members it published.
func (r *publicationRecord) promote() {
	priorGUID, priorIndex := r.netConf.PriorPublishedPfGUID, r.netConf.PriorPublishedDeviceIndex
	r.netConf.PublicationUnconfirmed = false
	r.netConf.PriorPublishedPfGUID, r.netConf.PriorPublishedDeviceIndex = "", 0
	err := utils.SaveNetConf(r.args.ContainerID, config.DefaultCNIDir, r.args.IfName, r.netConf)
	if err != nil {
		r.netConf.PublicationUnconfirmed = true
		r.netConf.PriorPublishedPfGUID, r.netConf.PriorPublishedDeviceIndex = priorGUID, priorIndex
		klog.ErrorS(err, "publication record not promoted to confirmed; it stays intent, which its DEL settles before withdrawing",
			"container", r.args.ContainerID, "interface", r.args.IfName)
	}
}

// recordedPublication is the configuration the ADD for this sandbox and
// interface cached when it published. Absent means that ADD never got as far
// as publishing, or the record is gone. Anything else that stops it being read
// is indeterminate, not absence: a record that cannot be read does not prove
// nothing was published.
func recordedPublication(args *skel.CmdArgs) (*localtypes.NetConf, bool) {
	cached, err := readRecordedPublication(args)
	return cached, err == nil && cached != nil
}

// errNoRecord is the definite absence of this sandbox's record.
var errNoRecord = errors.New("no publication record for this sandbox")

func readRecordedPublication(args *skel.CmdArgs) (*localtypes.NetConf, error) {
	path := filepath.Join(config.DefaultCNIDir, args.ContainerID+"-"+args.IfName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, errNoRecord
	}
	return readRecord(path)
}

// readRecord reads one publication record; the caller decides what an
// unreadable record means.
func readRecord(path string) (*localtypes.NetConf, error) {
	data, err := utils.ReadScratchNetConf(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	record := &localtypes.NetConf{}
	if err := json.Unmarshal(data, record); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return record, nil
}

// publicationInstant is the instant a record was published at. A record
// without one, or with one that does not parse, cannot be ordered.
func publicationInstant(conf *localtypes.NetConf) (time.Time, error) {
	if conf.PublishedAt == "" {
		return time.Time{}, errors.New("record carries no publication instant")
	}
	at, err := time.Parse(time.RFC3339Nano, conf.PublishedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("record carries an unreadable publication instant %q: %w", conf.PublishedAt, err)
	}
	return at, nil
}

// laterPublication looks at every record another sandbox of the same Pod
// made of the same function after own did: the name of a confirmed one, if
// any, and how many are unconfirmed intent. kubelet can tear an earlier
// sandbox down after a later one has been set up, and the device plugin hands
// the Pod the same functions, so the late DEL names the key the live sandbox
// published; a confirmed later record means the member is that sandbox's.
// Ownership is decided from all later records, never from the first file the
// directory lists: an unconfirmed intent must not hide a confirmed owner.
// A record that cannot be read is passed over and logged: it identifies no
// attachment, so it settles nothing, and refusing to proceed would let one
// damaged file stop every publishing DEL on the node. That it might have been
// the later sandbox's is an accepted risk, kept small by the atomic writer,
// which never leaves a torn record. Two records of the attachment are
// ordered by their instants, and a record of the attachment without a
// readable instant cannot be ordered, which is an error: a withdrawal is
// destructive, and that record may be the live sandbox's. A sandbox alone with
// its record needs no instant to withdraw.
func laterPublication(args *skel.CmdArgs, own *localtypes.NetConf) (confirmed string, pending int, err error) {
	if own.PodUID == "" {
		return "", 0, nil
	}
	entries, err := os.ReadDir(config.DefaultCNIDir)
	if err != nil {
		return "", 0, fmt.Errorf("list %s: %w", config.DefaultCNIDir, err)
	}
	ownRef := args.ContainerID + "-" + args.IfName
	var ownAt time.Time
	ownOrdered := false
	for _, entry := range entries {
		other := sameFunctionRecord(entry, ownRef, own)
		if other == nil {
			continue
		}
		if !ownOrdered {
			if ownAt, err = publicationInstant(own); err != nil {
				return "", 0, fmt.Errorf("this sandbox's %w", err)
			}
			ownOrdered = true
		}
		otherAt, err := publicationInstant(other)
		if err != nil {
			return "", 0, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if !otherAt.After(ownAt) {
			continue
		}
		if other.PublicationUnconfirmed {
			pending++
		} else if confirmed == "" {
			confirmed = entry.Name()
		}
	}
	return confirmed, pending, nil
}

// sameFunctionRecord reads one cache entry and returns it when it is another
// sandbox's record of own's Pod and function; nil for everything else.
// Atomic writers can be between create and rename, or have crashed: staging
// files never establish ownership, even if their JSON is complete. A record
// that cannot be read is logged and passed over, as laterPublication explains.
func sameFunctionRecord(entry os.DirEntry, ownRef string, own *localtypes.NetConf) *localtypes.NetConf {
	if entry.IsDir() || entry.Name() == ownRef || strings.HasPrefix(entry.Name(), utils.NetConfTempPrefix) {
		return nil
	}
	other, err := readRecord(filepath.Join(config.DefaultCNIDir, entry.Name()))
	if err != nil {
		klog.ErrorS(err, "publication record passed over while deciding ownership: it cannot be read and so identifies no attachment")
		return nil
	}
	if other.PodUID != own.PodUID || other.DeviceID != own.DeviceID {
		return nil
	}
	return other
}

// publication is what an ADD learned by publishing: the attachment key it
// wrote and the UID of the Pod it wrote it to. The UID comes from the Pod that
// was read and verified before the device was touched, not from CNI_ARGS, which
// an older runtime may leave without one; the wait fences on it either way.
type publication struct {
	Key    string
	UID    string
	Detail connectiondetails.Detail
}

// awaitConnectionReady blocks this delegate until ib-kubernetes reports the
// attachment Ready, or fails the ADD when the budget runs out. Multus invokes
// delegates in order, so the wait holds back this Pod's later attachments and
// no others. Reached only through the publish path, which has already
// established that this delegate publishes, so publishing implies waiting.
// Only the Pod that was published to can satisfy it: a same-name replacement
// that appears mid-wait carries another UID and fails the ADD instead.
func awaitConnectionReady(parent context.Context, getter connectiondetails.PodGetter,
	args *skel.CmdArgs, netConf *localtypes.NetConf, published publication,
) (retErr error) {
	finish := observability.Phase(parent, "readiness_wait")
	defer func() { finish(retErr) }()
	namespace, name, _ := parsePodIdentity(args.Args)
	ctx, cancel := context.WithTimeout(parent, netConf.ReadinessTimeout())
	defer cancel()
	// This is observed convergence, including cache delivery and poll delay;
	// it is not a direct measurement of the controller's UFM call.
	started := time.Now()
	if err := connectiondetails.WaitForReady(ctx, getter, namespace, name, published.UID, published.Key); err != nil {
		return fmt.Errorf("%w (waited %s)", err, time.Since(started).Round(time.Millisecond))
	}
	klog.InfoS("connection-details ready", "namespace", namespace, "pod", name,
		"interface", args.IfName, "network", published.Key,
		"waited", time.Since(started).Round(time.Millisecond))
	return nil
}

// removeConnectionDetails withdraws the entry this delegate published, reading
// and writing the API server directly.
func removeConnectionDetails(ctx context.Context, args *skel.CmdArgs, netConf *localtypes.NetConf) error {
	client, err := connectiondetails.NewPodClient(netConf.Kubeconfig)
	if err != nil {
		return err
	}
	return removePodConnectionDetailsExpecting(ctx, client, args, netConf.DeviceID, netConf.PodUID, expectedMembers(netConf))
}

// removePodConnectionDetails resolves the entry the same way publication
// resolved it, so teardown removes exactly its own member and leaves every
// sibling untouched.
//
// The Pod is identified by UID: from CNI_ARGS when the runtime passes one, and
// otherwise from recordedUID, the UID the ADD saw and kept with its cached
// configuration. With neither there is no evidence which Pod this attachment
// belonged to. The device plugin may have handed the same address to a
// same-name replacement Pod since, and removing whichever member holds the
// address now would take that Pod's published allocation away, so the
// withdrawal is skipped and DEL still succeeds.
func removePodConnectionDetails(parent context.Context, client podAnnotationClient,
	args *skel.CmdArgs, deviceID, recordedUID string,
) error {
	return removePodConnectionDetailsExpecting(parent, client, args, deviceID, recordedUID, nil)
}

// removePodConnectionDetailsExpecting is removePodConnectionDetails for a
// sandbox that may withdraw only members it published: expect lists them, the
// authoritative copy is read directly, and only a member equal to one of them
// is withdrawn. A member that differs belongs to another sandbox and is left
// where it is.
func removePodConnectionDetailsExpecting(parent context.Context, client podAnnotationClient,
	args *skel.CmdArgs, deviceID, recordedUID string, expect []connectiondetails.Detail,
) error {
	namespace, name, uid := parsePodIdentity(args.Args)
	if uid == "" {
		uid = recordedUID
	}
	observability.SetIdentity(parent, observability.Identity{
		Namespace: namespace, Pod: name, UID: uid, Interface: args.IfName, DeviceID: deviceID,
	})
	if namespace == "" || name == "" {
		return fmt.Errorf("pod identity missing from CNI_ARGS")
	}
	if err := connectiondetails.ValidateDeviceID(deviceID); err != nil {
		return err
	}
	if uid == "" {
		klog.InfoS("connection-details withdrawal skipped: no Pod UID in CNI_ARGS or in the cached configuration, "+
			"and a same-name Pod is not edited without evidence", "namespace", namespace, "pod", name,
			"interface", args.IfName, "device", deviceID)
		return nil
	}
	ctx, cancel := connectiondetails.PublishContext(parent)
	defer cancel()
	read, confirmed := client.GetPod, false
	if len(expect) > 0 {
		read, confirmed = client.GetPodAuthoritative, true
	}
	return retryConflicts(func() error {
		pod, value, changed, err := plannedWithdrawal(ctx, client, read, confirmed,
			namespace, name, uid, deviceID, expect)
		if err != nil || pod == nil || !changed {
			return err
		}
		return patchObservedAnnotation(ctx, client, namespace, name, pod, value)
	})
}

// plannedWithdrawal reads the Pod through read and works out what removing
// deviceID would change. A nil Pod means there is nothing left to withdraw
// from, which DEL treats as done.
//
// A Pod that is being deleted is left as it is. Its details are the
// controller's record of what to release, and a controller that is slow, or
// restarts, must still find them there; the Pod object takes them along when
// it goes, so nothing is left behind. Withdrawal is for the other DEL: the
// rollback of a failed ADD on a Pod that lives on and will be retried. The
// deletion is checked on whatever copy is read, and a write fenced on an
// older copy is rejected once the deletion lands, so the retry reads live and
// sees it.
//
// When the read says there is nothing to remove, the same question is put to
// the API server before the answer is believed. A record published after this
// copy was taken would otherwise be stranded: it would outlive the attachment
// and go on claiming it, with no later call to clean it up.
func plannedWithdrawal(ctx context.Context, client podAnnotationClient,
	read func(context.Context, string, string) (*corev1.Pod, error), confirmed bool,
	namespace, name, uid, deviceID string, expect []connectiondetails.Detail,
) (*corev1.Pod, string, bool, error) {
	pod, err := read(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	if uid != "" && string(pod.UID) != uid {
		if !confirmed {
			return plannedWithdrawal(ctx, client, client.GetPodAuthoritative, true,
				namespace, name, uid, deviceID, expect)
		}
		return nil, "", false, nil
	}
	// Only after the Pod is known to be the one this DEL is for: a cached copy
	// of a deleted predecessor with the same name must not speak for its
	// live successor.
	if pod.DeletionTimestamp != nil {
		klog.InfoS("connection-details left in place: the pod is being deleted and takes them with it",
			"namespace", namespace, "pod", name, "device", deviceID)
		return nil, "", false, nil
	}
	if len(expect) > 0 {
		if member, present := memberForDevice(pod.Annotations[connectiondetails.Annotation], deviceID); present && !ownMember(member, expect) {
			klog.InfoS("connection-details withdrawal skipped: the member published for this function is not one "+
				"this sandbox published, so another sandbox owns it",
				"namespace", namespace, "pod", name, "device", deviceID)
			return pod, "", false, nil
		}
	}
	value, changed, err := connectiondetails.RemoveAnnotationForDevice(
		pod.Annotations[connectiondetails.Annotation], deviceID)
	if err != nil {
		return nil, "", false, err
	}
	if changed {
		return pod, value, true, nil
	}
	if confirmed {
		return pod, value, false, nil
	}
	return plannedWithdrawal(ctx, client, client.GetPodAuthoritative, true, namespace, name, uid, deviceID, expect)
}

// deviceReader binds PF inspection to one command's context, because it waits
// for the node's CNI lock before it reaches the device.
func deviceReader(ctx context.Context) deviceResolver {
	return func(deviceID, knownGUID string) (int, string, error) { return readPFDetail(ctx, deviceID, knownGUID) }
}

// deviceResolver returns the index and GUID for one function. knownGUID is a
// GUID this Pod's retained Ready already carries for the function, or empty;
// a resolver that trusts it skips the device.
type deviceResolver func(deviceID, knownGUID string) (int, string, error)

// knownPFDetail satisfies a resolution from a GUID that is already known,
// checking only that the address still holds an InfiniBand function.
func knownPFDetail(ctx context.Context, deviceID, guid, source string, index func(string) (int, error)) (int, string, error) {
	started := time.Now()
	finish := observability.Phase(ctx, "guid_cache")
	i, err := index(deviceID)
	finish(err)
	if err != nil {
		return 0, "", fmt.Errorf("failed to determine IB device index for %s: %w", deviceID, err)
	}
	klog.InfoS("pf guid resolved", "device", deviceID, "source", source, "guid", guid,
		"elapsed", time.Since(started).Round(time.Millisecond))
	return i, guid, nil
}

// cachedPFDetail answers from the daemon's GUID cache when it can. The index
// is always recomputed: it is a cheap sysfs walk that also proves the address
// still holds an InfiniBand function, which is the one sanity check a cached
// GUID needs. A miss reads the device inside this ADD, the only moment the
// function is the Pod's rather than the device plugin's, and remembers it. A
// hit is its own phase, so the histogram shows the read that was not made
// instead of a device_inspection that never happened.
func cachedPFDetail(ctx context.Context, cache *guidcache.Cache, deviceID, knownGUID string,
	index func(string) (int, error), read deviceResolver,
) (int, string, error) {
	// A GUID the Pod's own retained Ready carries is the most specific memory
	// there is: it was read from this function for this Pod. It also seeds
	// the daemon's cache, so a daemon restarted mid-retry learns it too.
	if knownGUID != "" {
		i, guid, err := knownPFDetail(ctx, deviceID, knownGUID, "status", index)
		if err == nil && cache != nil {
			cache.Store(deviceID, guid)
		}
		return i, guid, err
	}
	if cache != nil {
		if guid, ok := cache.Lookup(deviceID); ok {
			return knownPFDetail(ctx, deviceID, guid, "cache", index)
		}
	}
	started := time.Now()
	i, guid, err := read(deviceID, "")
	if err != nil {
		return 0, "", err
	}
	if cache != nil {
		cache.Store(deviceID, guid)
	}
	klog.InfoS("pf guid resolved", "device", deviceID, "source", "device", "guid", guid,
		"elapsed", time.Since(started).Round(time.Millisecond))
	return i, guid, nil
}

// Hold the same lock as VF setup only while inspecting/rebinding the device,
// never while waiting for Kubernetes. ReadPFNodeGUID restores vfio-pci on errors.
func readPFDetail(ctx context.Context, deviceID, knownGUID string) (_ int, _ string, retErr error) {
	// The plugin process has no cache, but a retry's Pod still carries the
	// GUID the controller bound; that is enough to skip the rebind here too.
	if knownGUID != "" {
		return knownPFDetail(ctx, deviceID, knownGUID, "status", connectiondetails.IBDeviceIndex)
	}
	lock, err := lockCNIExecution(ctx, "device_lock")
	if err != nil {
		return 0, "", err
	}
	defer unlockCNIExecution(lock)
	finish := observability.Phase(ctx, "device_inspection")
	defer func() { finish(retErr) }()
	finishIndex := observability.Detail(ctx, "index_scan")
	index, err := connectiondetails.IBDeviceIndex(deviceID)
	finishIndex(err)
	if err != nil {
		return 0, "", fmt.Errorf("failed to determine IB device index for %s: %w", deviceID, err)
	}
	guid, err := connectiondetails.ReadPFNodeGUIDObserved(ctx, deviceID)
	if err != nil {
		return 0, "", fmt.Errorf("failed to read PF GUID for %s: %w", deviceID, err)
	}
	return index, guid, nil
}

// errPodUIDMismatch marks a Pod read whose UID is not the one this command is
// for: a cached predecessor of the same name, or a replacement.
var errPodUIDMismatch = errors.New("UID mismatch")

func resolvePodDetail(pod *corev1.Pod, uid, ifName, deviceID, networkName string) (connectiondetails.Detail, error) {
	if uid != "" && string(pod.UID) != uid {
		return connectiondetails.Detail{}, fmt.Errorf("pod %s/%s %w: expected %s, found %s",
			pod.Namespace, pod.Name, errPodUIDMismatch, uid, pod.UID)
	}
	if _, err := connectiondetails.Parse(pod.Annotations[connectiondetails.Annotation]); err != nil {
		return connectiondetails.Detail{}, err
	}
	namespace, name, err := connectiondetails.ResolveNetwork(pod.Namespace,
		pod.Annotations[connectiondetails.NetworksAnnotation], ifName, networkName)
	if err != nil {
		return connectiondetails.Detail{}, err
	}
	return connectiondetails.Detail{NameSpace: namespace, NetworkName: name, DeviceID: deviceID}, nil
}

// Publish exactly the allocation of this delegate. The identity GET and the
// merge loop run under separate API budgets: the PF rebind between them waits
// for the CNI lock and takes seconds per delegate, so charging it to one shared
// deadline would fail ADD whenever several Pods start on the node at once.
func publishPodConnectionDetails(parent context.Context, client podAnnotationClient, args *skel.CmdArgs,
	deviceID string, readDevice deviceResolver,
) (publication, error) {
	return publishPodConnectionDetailsRecording(parent, client, args, deviceID, readDevice, nil)
}

// publishPodConnectionDetailsRecording is publishPodConnectionDetails with a
// record step: once the Pod is verified and the device read, the record is
// written and then the publication, both under the ownership lock, which is
// released before this returns. A nil record publishes without one.
func publishPodConnectionDetailsRecording(parent context.Context, client podAnnotationClient, args *skel.CmdArgs,
	deviceID string, readDevice deviceResolver, record *publicationRecord,
) (_ publication, retErr error) {
	publishStarted := time.Now()
	finishIdentity := observability.Phase(parent, "config_identity")
	defer func() { finishIdentity(retErr) }()
	namespace, name, uid := parsePodIdentity(args.Args)
	observability.SetIdentity(parent, observability.Identity{
		Namespace: namespace, Pod: name, UID: uid, Interface: args.IfName, DeviceID: deviceID,
	})
	if namespace == "" || name == "" {
		return publication{}, fmt.Errorf("pod identity missing from CNI_ARGS")
	}
	if err := connectiondetails.ValidateDeviceID(deviceID); err != nil {
		return publication{}, err
	}
	networkName := configuredNetworkName(args.StdinData)
	detail, uid, retained, err := resolvePublication(parent, client, namespace, name, uid, args.IfName, deviceID, networkName)
	if err != nil {
		return publication{}, err
	}
	observability.SetIdentity(parent, observability.Identity{UID: uid})
	finishIdentity(nil)
	detail.DeviceIndex, detail.PfGUID, err = readDevice(deviceID, retained)
	if err != nil {
		return publication{}, err
	}
	published := publication{Key: detail.Key(), UID: uid, Detail: detail}
	if err := recordThenPublish(parent, client, args, namespace, name, uid, networkName, detail, published, record); err != nil {
		return publication{}, err
	}
	klog.InfoS("connection-details published or current", "namespace", namespace, "pod", name,
		"interface", args.IfName, "device", deviceID, "network", detail.Key(),
		"index", detail.DeviceIndex, "guid", detail.PfGUID,
		"elapsed", time.Since(publishStarted).Round(time.Millisecond))
	return published, nil
}

// recordThenPublish holds the ownership lock across the record and the
// publication write. With no record to make, the lock is still taken: a DEL
// deciding ownership must not interleave with any publication of the same key.
// The record is written first, as intent, so a process that dies at the write
// leaves no claim of ownership; it is promoted to a confirmed publication when
// the write is acknowledged, and settleRecord decides, under the same lock, what
// a failed write leaves behind: the previous record restored, nothing, intent,
// or a confirmed publication.
func recordThenPublish(parent context.Context, client podAnnotationClient, args *skel.CmdArgs,
	namespace, name, uid, networkName string, detail connectiondetails.Detail, published publication,
	record *publicationRecord,
) error {
	lock, err := acquireOwnershipLock(parent)
	if err != nil {
		return err
	}
	defer unlockCNIExecution(lock)
	var previous *localtypes.NetConf
	if record != nil {
		previous, _ = recordedPublication(args)
		if err := record.record(published, previous); err != nil {
			return err
		}
	}
	finishMerge := observability.Phase(parent, "annotation_merge")
	err = mergePublication(parent, client, namespace, name, uid, args.IfName, networkName, detail)
	if record != nil {
		if err == nil {
			record.promote()
		} else {
			err = settleRecord(parent, client, args, namespace, name, uid, networkName, detail, record, previous, err)
		}
	}
	finishMerge(err)
	return err
}

// settleRecord decides what this sandbox's record stands for after the
// publication write failed. A rejection the API server reported means nothing
// was written: the record goes back to what it was. Any other failure leaves
// the outcome unknown, so the authoritative copy is read: a Pod that carries
// the member confirms the write, the record is promoted and the publication
// stands; one that does not means it was not applied and the record is undone;
// a read that fails leaves the record as the intent it already is, which this
// sandbox's DEL settles against the authoritative copy before it withdraws
// anything. The read is bounded by the publication budget and survives the
// caller's cancellation: it decides what the next DEL may do.
func settleRecord(parent context.Context, client podAnnotationClient, args *skel.CmdArgs,
	namespace, name, uid, networkName string, detail connectiondetails.Detail,
	record *publicationRecord, previous *localtypes.NetConf, failure error,
) error {
	var unacknowledged unacknowledgedWriteError
	if !errors.As(failure, &unacknowledged) {
		undoRecord(args, previous)
		return failure
	}
	ctx, cancel := connectiondetails.PublishContext(context.WithoutCancel(parent))
	defer cancel()
	pod, err := client.GetPodAuthoritative(ctx, namespace, name)
	if err == nil {
		var resolved connectiondetails.Detail
		if resolved, err = resolvePodDetail(pod, uid, args.IfName, detail.DeviceID, networkName); err == nil {
			var current bool
			if current, err = publicationIsCurrent(pod, resolved, detail); err == nil {
				if current {
					klog.InfoS("connection-details publication confirmed after an unacknowledged write",
						"namespace", namespace, "pod", name, "container", args.ContainerID, "network", detail.Key(),
						"writeError", failure.Error())
					record.promote()
					return nil
				}
				undoRecord(args, previous)
				return failure
			}
		}
	}
	klog.ErrorS(err, "connection-details publication outcome unknown: the write was not acknowledged and the "+
		"confirming read failed; the record stays unconfirmed intent, which this sandbox's DEL settles against "+
		"the authoritative copy before it withdraws anything",
		"namespace", namespace, "pod", name, "container", args.ContainerID, "network", detail.Key(),
		"writeError", failure.Error())
	return failure
}

// expectedMembers are the members a DEL may withdraw when it must be exactly
// what this sandbox published: for an unconfirmed record, the member it set
// out to publish and the member its previous confirmed publication left; for a
// confirmed record whose successor is unconfirmed, the member it published,
// since promotion forgets the previous one. Nil otherwise: a confirmed record
// owns whatever member its function carries.
func expectedMembers(netConf *localtypes.NetConf) []connectiondetails.Detail {
	if !netConf.PublicationUnconfirmed && !netConf.WithdrawExactMember {
		return nil
	}
	members := []connectiondetails.Detail{{
		DeviceID: netConf.DeviceID, PfGUID: netConf.PublishedPfGUID, DeviceIndex: netConf.PublishedDeviceIndex,
	}}
	if netConf.PriorPublishedPfGUID != "" {
		members = append(members, connectiondetails.Detail{
			DeviceID: netConf.DeviceID, PfGUID: netConf.PriorPublishedPfGUID, DeviceIndex: netConf.PriorPublishedDeviceIndex,
		})
	}
	return members
}

// memberForDevice is the member published for deviceID, if the annotation
// carries one it can read.
func memberForDevice(existing, deviceID string) (connectiondetails.Detail, bool) {
	details, err := connectiondetails.Parse(existing)
	if err != nil {
		return connectiondetails.Detail{}, false
	}
	for _, detail := range details {
		if detail.DeviceID == deviceID {
			return detail, true
		}
	}
	return connectiondetails.Detail{}, false
}

// ownMember reports whether member is one of the members this sandbox may
// withdraw.
func ownMember(member connectiondetails.Detail, expect []connectiondetails.Detail) bool {
	for _, e := range expect {
		if member.PfGUID == e.PfGUID && member.DeviceIndex == e.DeviceIndex {
			return true
		}
	}
	return false
}

// undoRecord puts this sandbox's record back to what it was before a
// publication that then failed: the record of the previous publication, or
// none. A previous record that could not be read is not restored; an absent
// record and an unreadable one both withdraw nothing.
func undoRecord(args *skel.CmdArgs, previous *localtypes.NetConf) {
	path := filepath.Join(config.DefaultCNIDir, args.ContainerID+"-"+args.IfName)
	var err error
	if previous != nil {
		err = utils.SaveNetConf(args.ContainerID, config.DefaultCNIDir, args.IfName, previous)
	} else {
		err = utils.CleanCachedNetConf(path)
	}
	if err != nil {
		klog.ErrorS(err, "publication record not undone after a failed publication; "+
			"this sandbox's DEL may act on a record of a publication that did not happen",
			"container", args.ContainerID, "interface", args.IfName, "path", path)
	}
}

// podForAttachment reads the Pod and resolves this command's attachment on it.
// A cached copy whose UID contradicts the one this ADD is for may be a
// same-name predecessor the informer has not dropped yet, so the authoritative
// copy is read once before the mismatch is believed, as the withdrawal does. A
// UID the authoritative copy also contradicts is a replacement and is an error.
func podForAttachment(ctx context.Context, client podAnnotationClient,
	namespace, name, uid, ifName, deviceID, networkName string,
) (*corev1.Pod, connectiondetails.Detail, error) {
	pod, err := client.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, connectiondetails.Detail{}, err
	}
	detail, err := resolvePodDetail(pod, uid, ifName, deviceID, networkName)
	if errors.Is(err, errPodUIDMismatch) {
		if pod, err = client.GetPodAuthoritative(ctx, namespace, name); err != nil {
			return nil, connectiondetails.Detail{}, err
		}
		detail, err = resolvePodDetail(pod, uid, ifName, deviceID, networkName)
	}
	return pod, detail, err
}

// publicationIsCurrent reports whether pod already carries detail, refusing a
// Pod whose network selection, resolved as resolved, no longer names the same
// attachment.
func publicationIsCurrent(pod *corev1.Pod, resolved, detail connectiondetails.Detail) (bool, error) {
	if resolved.Key() != detail.Key() {
		return false, fmt.Errorf("network selection changed during publication for %s/%s: %s now resolves as %s",
			pod.Namespace, pod.Name, detail.Key(), resolved.Key())
	}
	return connectiondetails.IsCurrent(pod.Annotations[connectiondetails.Annotation], detail), nil
}

// resolvePublication verifies the Pod and selects its network before any
// hardware is touched. It returns the observed Pod UID so retries fence Pod
// replacement even when an older Multus omitted K8S_POD_UID.
// It also returns the GUID the Pod's retained Ready carries for this
// attachment, when the controller echoed one, so a retry can republish
// without reading the device.
func resolvePublication(parent context.Context, client podAnnotationClient,
	namespace, name, uid, ifName, deviceID, networkName string,
) (connectiondetails.Detail, string, string, error) {
	ctx, cancel := connectiondetails.PublishContext(parent)
	defer cancel()
	pod, detail, err := podForAttachment(ctx, client, namespace, name, uid, ifName, deviceID, networkName)
	if err != nil {
		return connectiondetails.Detail{}, "", "", err
	}
	retained, _ := connectiondetails.RetainedGUID(pod.Annotations[connectiondetails.StatusAnnotation], detail.Key())
	return detail, string(pod.UID), retained, nil
}

// mergePublication merges one record under resourceVersion fencing. Each retry
// reads a coherent Pod snapshot and merges against it, preserving other
// writers. DefaultBackoff spaces the retries: a starting Pod is updated by
// other controllers, and any such update fails the resourceVersion test.
func mergePublication(parent context.Context, client podAnnotationClient,
	namespace, name, uid, ifName, networkName string, detail connectiondetails.Detail,
) error {
	ctx, cancel := connectiondetails.PublishContext(parent)
	defer cancel()
	return retryConflicts(func() error {
		latest, resolved, err := podForAttachment(ctx, client, namespace, name, uid, ifName, detail.DeviceID, networkName)
		if err != nil {
			return err
		}
		current, err := publicationIsCurrent(latest, resolved, detail)
		if err != nil {
			return err
		}
		if current {
			// A copy that already shows this record cannot tell "published"
			// from "published, then withdrawn while this copy aged". Reporting
			// success on the second leaves the attachment unannounced with
			// nothing left to announce it. Confirm before agreeing, and
			// publish from the authoritative copy when they disagree.
			if latest, err = client.GetPodAuthoritative(ctx, namespace, name); err != nil {
				return err
			}
			if resolved, err = resolvePodDetail(latest, uid, ifName, detail.DeviceID, networkName); err != nil {
				return err
			}
			if current, err = publicationIsCurrent(latest, resolved, detail); err != nil {
				return err
			}
			if current {
				return nil
			}
		}
		value, mergeErr := connectiondetails.MergeAnnotation(
			latest.Annotations[connectiondetails.Annotation], detail)
		if mergeErr != nil {
			return mergeErr
		}
		return patchObservedAnnotation(ctx, client, namespace, name, latest, value)
	})
}

// configuredNetworkName is the name field of the configuration Multus passed
// for this delegate: the network definition's own configuration, written by
// the definition's owner. It is read on its own so a configuration this
// process could not fully parse still identifies itself the way ADD saw it.
func configuredNetworkName(stdin []byte) string {
	var conf struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(stdin, &conf)
	return conf.Name
}

func patchObservedAnnotation(ctx context.Context, client podAnnotationClient, namespace, name string,
	pod *corev1.Pod, value string,
) error {
	err := client.PatchAnnotation(ctx, namespace, name, value, pod.ResourceVersion, pod.Annotations != nil)
	observability.Patch(ctx, err, apierrors.IsConflict(err) || apierrors.IsInvalid(err))
	if err != nil && !clientRejected(err) {
		return unacknowledgedWriteError{err: err}
	}
	return err
}

// unacknowledgedWriteError is a write whose outcome the API server did not report:
// a transport failure, a canceled or expired request, or a server-side error.
// The write may have been applied. A rejection the server reported, a 4xx
// status, is never wrapped: the server answered and applied nothing.
type unacknowledgedWriteError struct{ err error }

func (u unacknowledgedWriteError) Error() string { return u.err.Error() }
func (u unacknowledgedWriteError) Unwrap() error { return u.err }

// clientRejected reports whether err is the API server refusing the request
// with a client-error status, which is the one outcome that proves nothing
// was written.
func clientRejected(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	return code >= http.StatusBadRequest && code < http.StatusInternalServerError
}

// retryConflicts runs fn until it succeeds, fails with something other than a
// conflict or a validation rejection, or client-go's default backoff is spent.
// client-go's retry.OnError treats a canceled or expired context as the loop
// ending and reports the last retriable error in its place: nil when the
// interrupted attempt was the first, so a canceled write would read as
// success, and an earlier conflict otherwise, which would make the final
// attempt's unknown outcome look like a definite rejection. The final
// attempt's own error is what the caller gets, in every case.
func retryConflicts(fn func() error) error {
	var attempt error
	_ = retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsInvalid(err)
	}, func() error {
		attempt = fn()
		return attempt
	})
	return attempt
}
