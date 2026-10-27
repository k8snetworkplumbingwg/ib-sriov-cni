package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

const testNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

type annotationClientFake struct {
	t                 *testing.T
	pod               *corev1.Pod
	getErr            error
	getCalls, patches int
	patchHook         func() error
	// Deadlines observed on the first GET and on the PATCH, to prove the two
	// API phases are budgeted separately from the device read between them.
	getDeadline, patchDeadline time.Time
}

func (f *annotationClientFake) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	f.t.Helper()
	f.getCalls++
	if deadline, ok := ctx.Deadline(); ok && f.getDeadline.IsZero() {
		f.getDeadline = deadline
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if namespace != f.pod.Namespace || name != f.pod.Name {
		f.t.Fatalf("GET targeted %s/%s, want %s/%s", namespace, name, f.pod.Namespace, f.pod.Name)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.pod.DeepCopy(), nil
}

// The fake reads one object, so an authoritative read is the same read. Tests
// that need the cached and live copies to diverge use their own client.
func (f *annotationClientFake) GetPodAuthoritative(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	f.t.Helper()
	return f.GetPod(ctx, namespace, name)
}

func (f *annotationClientFake) PatchAnnotation(ctx context.Context, namespace, name, value, rv string, annotationsPresent bool) error {
	f.t.Helper()
	f.patches++
	if deadline, ok := ctx.Deadline(); ok {
		f.patchDeadline = deadline
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if namespace != f.pod.Namespace || name != f.pod.Name || rv != f.pod.ResourceVersion {
		f.t.Fatalf("PATCH targeted %s/%s rv=%s, want %s/%s rv=%s", namespace, name, rv,
			f.pod.Namespace, f.pod.Name, f.pod.ResourceVersion)
	}
	if annotationsPresent != (f.pod.Annotations != nil) {
		f.t.Fatalf("annotationsPresent=%v does not match Pod", annotationsPresent)
	}
	if f.patchHook != nil {
		if err := f.patchHook(); err != nil {
			return err
		}
	}
	if f.pod.Annotations == nil {
		f.pod.Annotations = map[string]string{}
	}
	f.pod.Annotations[connectiondetails.Annotation] = value
	f.pod.ResourceVersion = strconv.Itoa(f.patches + 1)
	return nil
}

func newAnnotationTest(t *testing.T) (*annotationClientFake, *skel.CmdArgs) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "ns1", Name: "vm-1", UID: "uid-1", ResourceVersion: "1",
		Annotations: map[string]string{
			testNetworksAnnotation: `[{"name":"net-a","interface":"ib1"}]`,
			"example.com/other":    "keep",
		},
	}}
	args := &skel.CmdArgs{
		Args:   "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1;K8S_POD_UID=uid-1",
		IfName: "ib1", StdinData: []byte(`{"name":"net-a"}`),
	}
	return &annotationClientFake{t: t, pod: pod}, args
}

func annotationEntry(network, bdf string, index int) connectiondetails.Detail {
	return connectiondetails.Detail{
		NameSpace: "ns1", NetworkName: network, DeviceID: bdf,
		PfGUID: "90:e3:17:03:00:fb:91:8e", DeviceIndex: index,
	}
}

func encodeEntries(t *testing.T, entries map[string]connectiondetails.Detail) string {
	t.Helper()
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func checkEntry(t *testing.T, f *annotationClientFake, want connectiondetails.Detail, count int) {
	t.Helper()
	got, err := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	key := want.NameSpace + "/" + want.NetworkName + "/" + want.DeviceID
	if len(got) != count || got[key] != want {
		t.Fatalf("details=%+v, want %d entries including %+v at %s", got, count, want, key)
	}
	if f.pod.Annotations["example.com/other"] != "keep" {
		t.Fatal("unrelated annotation was changed")
	}
}

func TestPublishPodConnectionDetailsMerge(t *testing.T) {
	for _, existingNetwork := range []string{"", "net-b", "net-a"} {
		t.Run(existingNetwork, func(t *testing.T) {
			f, args := newAnnotationTest(t)
			sibling := annotationEntry(existingNetwork, "0002:03:00.0", 1)
			count := 1
			if existingNetwork != "" {
				count++
				f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{
					"ns1/" + existingNetwork + "/0002:03:00.0": sibling,
				})
			}
			if existingNetwork == "net-a" {
				f.pod.Annotations[testNetworksAnnotation] = `[{"name":"net-a","interface":"ib1"},{"name":"net-a","interface":"ib2"}]`
			}
			reads := 0
			_, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0", func(bdf, _ string) (int, string, error) {
				reads++
				if bdf != "0000:03:00.0" || f.getCalls == 0 {
					t.Fatalf("device read %s before Pod verification", bdf)
				}
				return 0, "90:e3:17:03:00:fb:91:8e", nil
			})
			if err != nil || reads != 1 || f.patches != 1 {
				t.Fatalf("err=%v reads=%d patches=%d", err, reads, f.patches)
			}
			checkEntry(t, f, annotationEntry("net-a", "0000:03:00.0", 0), count)
			if existingNetwork != "" {
				checkEntry(t, f, sibling, count)
			}
		})
	}
}

func TestPublishPodConnectionDetailsCurrent(t *testing.T) {
	f, args := newAnnotationTest(t)
	want := annotationEntry("net-a", "0000:03:00.0", 0)
	f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{
		"ns1/net-a/0000:03:00.0": want,
	})
	reads := 0
	_, err := publishPodConnectionDetails(t.Context(), f, args, want.DeviceID, func(string, string) (int, string, error) {
		reads++
		return want.DeviceIndex, want.PfGUID, nil
	})
	if err != nil || reads != 1 || f.patches != 0 {
		t.Fatalf("err=%v reads=%d patches=%d; want one identity read and no PATCH", err, reads, f.patches)
	}
}

func TestPublishPodConnectionDetailsRejectsBeforeDeviceRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*annotationClientFake, *skel.CmdArgs)
	}{
		{"missing identity", func(_ *annotationClientFake, args *skel.CmdArgs) { args.Args = "" }},
		{"UID mismatch", func(f *annotationClientFake, _ *skel.CmdArgs) { f.pod.UID = "replacement" }},
		{"ambiguous network", func(f *annotationClientFake, _ *skel.CmdArgs) {
			f.pod.Annotations[testNetworksAnnotation] = `[{"name":"net-a","interface":"ib1"},{"name":"net-b","interface":"ib1"}]`
		}},
		{"legacy flat annotation", func(f *annotationClientFake, _ *skel.CmdArgs) {
			f.pod.Annotations[connectiondetails.Annotation] = `{"0":"90:e3:17:03:00:fb:91:8e"}`
		}},
		{"malformed annotation", func(f *annotationClientFake, _ *skel.CmdArgs) {
			f.pod.Annotations[connectiondetails.Annotation] = `{"broken"`
		}},
		{"GET denied", func(f *annotationClientFake, _ *skel.CmdArgs) {
			f.getErr = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("denied"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, args := newAnnotationTest(t)
			tc.change(f, args)
			reads := 0
			_, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0", func(string, string) (int, string, error) {
				reads++
				return 0, "90:e3:17:03:00:fb:91:8e", nil
			})
			if err == nil || reads != 0 || f.patches != 0 {
				t.Fatalf("err=%v reads=%d patches=%d; want rejection before device read", err, reads, f.patches)
			}
		})
	}
}

func TestPublishPodConnectionDetailsBudgetsAPIPhasesSeparately(t *testing.T) {
	f, args := newAnnotationTest(t)
	const readDelay = 50 * time.Millisecond
	_, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0", func(string, string) (int, string, error) {
		time.Sleep(readDelay)
		return 0, "90:e3:17:03:00:fb:91:8e", nil
	})
	if err != nil || f.patches != 1 {
		t.Fatalf("err=%v patches=%d", err, f.patches)
	}
	if f.getDeadline.IsZero() || f.patchDeadline.IsZero() {
		t.Fatal("identity GET and merge PATCH must both run under a deadline")
	}
	if gap := f.patchDeadline.Sub(f.getDeadline); gap < readDelay {
		t.Fatalf("PATCH deadline only %v after GET deadline; the %v device read was charged to the API budget",
			gap, readDelay)
	}
}

func TestPublishPodConnectionDetailsRejectsMalformedDeviceID(t *testing.T) {
	for _, deviceID := range []string{"", "03:00.0", "0000:03:00.0/../../x", "0000:03:00.G", "../../tmp/x"} {
		f, args := newAnnotationTest(t)
		reads := 0
		_, err := publishPodConnectionDetails(t.Context(), f, args, deviceID, func(string, string) (int, string, error) {
			reads++
			return 0, "90:e3:17:03:00:fb:91:8e", nil
		})
		if err == nil || f.getCalls != 0 || reads != 0 || f.patches != 0 {
			t.Fatalf("deviceID %q: err=%v gets=%d reads=%d patches=%d; want rejection before any API call or device read",
				deviceID, err, f.getCalls, reads, f.patches)
		}
	}
}

func TestPublishPodConnectionDetailsReadError(t *testing.T) {
	f, args := newAnnotationTest(t)
	wantErr := errors.New("could not restore vfio-pci")
	_, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0", func(string, string) (int, string, error) {
		return 0, "", wantErr
	})
	if !errors.Is(err, wantErr) || f.patches != 0 {
		t.Fatalf("err=%v patches=%d; want reader failure without PATCH", err, f.patches)
	}
}

func checkPublicationFailure(t *testing.T, err error, patches int, retryable bool) {
	t.Helper()
	if err == nil {
		t.Fatal("persistent failure or Pod replacement succeeded")
	}
	if !retryable && patches != 1 {
		t.Fatalf("unexpected PATCH retry: %d", patches)
	}
	if retryable && (patches < 2 || patches > 20) {
		t.Fatalf("conflict attempts=%d; want bounded retry", patches)
	}
}

func TestPublishPodConnectionDetailsPatchFailures(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("changed"))
	invalid := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "vm-1", nil)
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "vm-1", errors.New("denied"))
	for _, tc := range []struct {
		name                    string
		patchErr                error
		persistent, replacement bool
		omitUID                 bool
	}{
		{name: "409 remerges sibling", patchErr: conflict},
		{name: "422 remerges sibling", patchErr: invalid},
		{name: "forbidden does not retry", patchErr: denied, persistent: true},
		{name: "exhausted conflicts", patchErr: conflict, persistent: true},
		{name: "replacement during retry", patchErr: conflict, replacement: true},
		{name: "replacement without CNI UID", patchErr: conflict, replacement: true, omitUID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, args := newAnnotationTest(t)
			if tc.omitUID {
				args.Args = "K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1"
			}
			sibling := annotationEntry("net-a", "0002:03:00.0", 1)
			f.patchHook = func() error {
				if f.patches == 1 {
					f.pod.ResourceVersion = "2"
					f.pod.Annotations[connectiondetails.Annotation] = encodeEntries(t, map[string]connectiondetails.Detail{
						"ns1/net-a/0002:03:00.0": sibling,
					})
					if tc.replacement {
						f.pod.UID = "replacement"
					}
				}
				if f.patches == 1 || tc.persistent {
					return tc.patchErr
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			reads := 0
			_, err := publishPodConnectionDetails(ctx, f, args, "0000:03:00.0", func(string, string) (int, string, error) {
				reads++
				return 0, "90:e3:17:03:00:fb:91:8e", nil
			})
			if reads != 1 {
				t.Fatalf("device read %d times; retries must reuse the GUID", reads)
			}
			if tc.persistent || tc.replacement {
				checkPublicationFailure(t, err, f.patches, tc.patchErr == conflict && tc.persistent)
				return
			}
			if err != nil || f.patches != 2 || f.getCalls < 2 {
				t.Fatalf("err=%v gets=%d patches=%d; want re-GET and one retry", err, f.getCalls, f.patches)
			}
			checkEntry(t, f, annotationEntry("net-a", "0000:03:00.0", 0), 2)
			checkEntry(t, f, sibling, 2)
		})
	}
}

// awaitConnectionReady must be inert unless the handshake was explicitly
// enabled, so upgrading the plugin cannot make an existing NAD start blocking.
// Publishing implies waiting. The dropped publish-without-wait mode was the
// only thing an unset timeout used to select, so it now sizes the wait instead
// of disabling it.
func TestAwaitConnectionReadyHasNoOptOut(t *testing.T) {
	for name, seconds := range map[string]int{"unset": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			f, args := newAnnotationTest(t)
			f.pod.Annotations[connectiondetails.StatusAnnotation] =
				`{"ns1/net-a/0000:03:00.0":{"Status":"Ready"}}`
			conf := &localtypes.NetConf{}
			conf.ReadinessTimeoutSeconds = seconds
			before := f.getCalls
			if err := awaitConnectionReady(t.Context(), f, args, conf, publication{Key: "ns1/net-a/0000:03:00.0", UID: "uid-1"}); err != nil {
				t.Fatalf("ready attachment returned %v", err)
			}
			if f.getCalls == before {
				t.Fatal("handshake skipped; publishing must imply waiting")
			}
			if got := conf.ReadinessTimeout(); got != localtypes.DefaultReadinessTimeoutSeconds*time.Second {
				t.Fatalf("timeout = %v, want the default", got)
			}
		})
	}
}

func TestAwaitConnectionReadySucceedsOnReadyAndFailsOnTimeout(t *testing.T) {
	key := publication{Key: "ns1/net-a/0000:03:00.0", UID: "uid-1"}
	conf := &localtypes.NetConf{}
	conf.ReadinessTimeoutSeconds = 1

	ready, args := newAnnotationTest(t)
	ready.pod.Annotations[connectiondetails.StatusAnnotation] =
		`{"ns1/net-a/0000:03:00.0":{"Status":"Ready"}}`
	if err := awaitConnectionReady(t.Context(), ready, args, conf, key); err != nil {
		t.Fatalf("a Ready attachment must let the ADD proceed: %v", err)
	}

	never, args2 := newAnnotationTest(t)
	never.pod.Annotations[connectiondetails.StatusAnnotation] = `{}`
	err := awaitConnectionReady(t.Context(), never, args2, conf, key)
	if err == nil {
		t.Fatal("an attachment that never becomes Ready must fail the ADD, not succeed")
	}
}

// Publication returns the key it wrote so the wait watches the same attachment
// it just published, rather than one derived a second time.
func TestPublishReturnsTheKeyItPublished(t *testing.T) {
	f, args := newAnnotationTest(t)
	published, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0",
		func(string, string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil })
	if err != nil {
		t.Fatal(err)
	}
	if published.Key != "ns1/net-a/0000:03:00.0" || published.UID != "uid-1" {
		t.Fatalf("publication=%+v", published)
	}
	if _, parseErr := connectiondetails.Parse(f.pod.Annotations[connectiondetails.Annotation]); parseErr != nil {
		t.Fatal(parseErr)
	}
}

func TestReadinessTimeoutReportsHowLongItWaited(t *testing.T) {
	f, args := newAnnotationTest(t)
	conf := &localtypes.NetConf{}
	conf.ReadinessTimeoutSeconds = 1
	// Nothing ever reports ready, so the wait runs its full budget.
	err := awaitConnectionReady(context.Background(), f, args, conf, publication{Key: "ns1/net-a/0000:03:00.0", UID: "uid-1"})
	if err == nil {
		t.Fatal("the wait reported success with no ready status")
	}
	// The duration belongs in the error too: a timeout that does not say how
	// long it waited cannot be told apart from one that gave up instantly.
	if !strings.Contains(err.Error(), "waited ") {
		t.Fatalf("timeout error does not report the wait: %v", err)
	}
}

// After a rollback the details entry is gone but the Ready status stays, and
// since the controller echoes the GUID it bound, the retry republishes with
// it instead of rebinding the function to read it again.
func TestPublishReusesTheGUIDARetainedReadyCarries(t *testing.T) {
	f, args := newAnnotationTest(t)
	want := annotationEntry("net-a", "0000:03:00.0", 0)
	f.pod.Annotations[connectiondetails.StatusAnnotation] =
		`{"ns1/net-a/0000:03:00.0":{"Status":"Ready","pfGuid":"` + want.PfGUID + `"}}`
	var known string
	reads := 0
	_, err := publishPodConnectionDetails(t.Context(), f, args, want.DeviceID, func(_, knownGUID string) (int, string, error) {
		known = knownGUID
		if knownGUID != "" {
			return want.DeviceIndex, knownGUID, nil
		}
		reads++
		return want.DeviceIndex, want.PfGUID, nil
	})
	if err != nil || known != want.PfGUID || reads != 0 || f.patches != 1 {
		t.Fatalf("err=%v known=%q reads=%d patches=%d", err, known, reads, f.patches)
	}
	checkEntry(t, f, want, 1)
	// A Ready without a GUID, as an older controller writes it, still reads the device.
	f, args = newAnnotationTest(t)
	f.pod.Annotations[connectiondetails.StatusAnnotation] = `{"ns1/net-a/0000:03:00.0":{"Status":"Ready"}}`
	known = "x"
	if _, err := publishPodConnectionDetails(t.Context(), f, args, want.DeviceID, func(_, knownGUID string) (int, string, error) {
		known = knownGUID
		return want.DeviceIndex, want.PfGUID, nil
	}); err != nil || known != "" {
		t.Fatalf("err=%v known=%q, want an empty known GUID", err, known)
	}
}

// The bare comma form of the networks annotation names no interfaces, so
// Multus calls this plugin as net1 and net2. The configuration it passes
// carries the definition's name, which is enough to publish under the right
// definition without an interface in the selection.
func TestPublishResolvesAnImplicitInterfaceFromTheConfigurationName(t *testing.T) {
	f, args := newAnnotationTest(t)
	f.pod.Annotations[testNetworksAnnotation] = "net-a,net-a"
	args.IfName = "net1"
	args.StdinData = []byte(`{"cniVersion":"0.3.1","name":"net-a","type":"ib-sriov","deviceID":"0000:03:00.0"}`)
	published, err := publishPodConnectionDetails(t.Context(), f, args, "0000:03:00.0",
		func(string, string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil })
	if err != nil {
		t.Fatal(err)
	}
	if published.Key != "ns1/net-a/0000:03:00.0" {
		t.Fatalf("published under %q", published.Key)
	}
	checkEntry(t, f, annotationEntry("net-a", "0000:03:00.0", 0), 1)
}
