package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

// This exercises the production publication and readiness path, on a real
// informer, with only the device read injected. What it is for is the property
// no single-machine round can show: one daemon serving more than one Pod at a
// time. The runtime invokes one Pod's attachments in order, so two requests
// only overlap when two Pods start together.

func concurrencyPod(name string, interfaces ...string) *corev1.Pod {
	selections := make([]map[string]string, 0, len(interfaces))
	for _, ifName := range interfaces {
		selections = append(selections, map[string]string{"name": "net-a", "interface": ifName})
	}
	encoded, err := json.Marshal(selections)
	if err != nil {
		panic(err)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1", Name: name, UID: types.UID("uid-" + name), ResourceVersion: "1",
			Annotations: map[string]string{testNetworksAnnotation: string(encoded)},
		},
		Spec: corev1.PodSpec{NodeName: testNodeName},
	}
}

func attachmentArgs(pod, ifName string) *skel.CmdArgs {
	return &skel.CmdArgs{
		Args:      fmt.Sprintf("K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=%s;K8S_POD_UID=uid-%s", pod, pod),
		IfName:    ifName,
		StdinData: []byte(`{"name":"net-a"}`),
	}
}

func waitingNetConf(deviceID string) *localtypes.NetConf {
	conf := &localtypes.NetConf{}
	conf.DeviceID = deviceID
	conf.ReadinessTimeoutSeconds = 20
	return conf
}

// markReady writes the controller's side of the handshake for one attachment.
func markReady(t *testing.T, client *fake.Clientset, pod, key string) {
	t.Helper()
	ctx := context.Background()
	live, err := client.CoreV1().Pods("ns1").Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statuses := connectiondetails.Statuses{}
	if existing := live.Annotations[connectiondetails.StatusAnnotation]; existing != "" {
		if statuses, err = connectiondetails.ParseStatus(existing); err != nil {
			t.Fatal(err)
		}
	}
	statuses[key] = connectiondetails.Status{Status: connectiondetails.StatusReady}
	encoded, err := json.Marshal(statuses)
	if err != nil {
		t.Fatal(err)
	}
	live.Annotations[connectiondetails.StatusAnnotation] = string(encoded)
	if _, err = client.CoreV1().Pods("ns1").Update(ctx, live, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func detailKey(network, deviceID string) string {
	return connectiondetails.Detail{NameSpace: "ns1", NetworkName: network, DeviceID: deviceID}.Key()
}

func awaitPublished(t *testing.T, client *fake.Clientset, pod string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		live, err := client.CoreV1().Pods("ns1").Get(context.Background(), pod, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		details, err := connectiondetails.Parse(live.Annotations[connectiondetails.Annotation])
		if err != nil {
			t.Fatal(err)
		}
		if len(details) >= count {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pod %s never published %d records", pod, count)
}

func TestOneWaitingAttachmentDoesNotBlockAnotherPod(t *testing.T) {
	cacheDirAt(t)
	podA, podB := concurrencyPod("vm-a", "ib1"), concurrencyPod("vm-b", "ib1", "ib2")
	pods, client := startNodeCache(t, podA, podB)
	reader := nodePodReader{pods: pods, api: connectiondetails.NewPodClientForClientset(client)}
	device := func(bdf, _ string) (int, string, error) { return 0, "90:e3:17:03:00:fb:91:8e", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A's only attachment publishes and then waits; nothing will report it
	// ready until the end.
	aDone := make(chan error, 1)
	go func() {
		aDone <- daemonPublisher(ctx, reader, device)(attachmentArgs("vm-a", "ib1"), waitingNetConf("0000:03:00.0"))
	}()
	awaitPublished(t, client, "vm-a", 1)

	// While A waits, B's two attachments must each complete in turn.
	for _, attachment := range []struct{ ifName, device string }{
		{"ib1", "0002:03:00.0"},
		{"ib2", "0010:03:00.0"},
	} {
		done := make(chan error, 1)
		go func() {
			done <- daemonPublisher(ctx, reader, device)(
				attachmentArgs("vm-b", attachment.ifName), waitingNetConf(attachment.device))
		}()
		markReadyWhenPublished(t, client, "vm-b", attachment.device)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("vm-b %s: %v", attachment.ifName, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("vm-b %s never completed while vm-a was waiting", attachment.ifName)
		}
		select {
		case err := <-aDone:
			t.Fatalf("vm-a completed before it was reported ready: %v", err)
		default:
		}
	}

	markReady(t, client, "vm-a", detailKey("net-a", "0000:03:00.0"))
	select {
	case err := <-aDone:
		if err != nil {
			t.Fatalf("vm-a: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("vm-a never completed after it was reported ready")
	}

	// Each Pod holds exactly its own records. Concurrent requests sharing one
	// cache must not write into each other's annotations.
	for pod, want := range map[string]int{"vm-a": 1, "vm-b": 2} {
		live, err := client.CoreV1().Pods("ns1").Get(context.Background(), pod, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		details, err := connectiondetails.Parse(live.Annotations[connectiondetails.Annotation])
		if err != nil {
			t.Fatal(err)
		}
		if len(details) != want {
			t.Fatalf("pod %s holds %d records, want %d: %+v", pod, len(details), want, details)
		}
	}
}

// markReadyWhenPublished waits for one attachment's record to appear and then
// reports it ready, which is what the controller does.
func markReadyWhenPublished(t *testing.T, client *fake.Clientset, pod, deviceID string) {
	t.Helper()
	key := detailKey("net-a", deviceID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		live, err := client.CoreV1().Pods("ns1").Get(context.Background(), pod, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		details, err := connectiondetails.Parse(live.Annotations[connectiondetails.Annotation])
		if err != nil {
			t.Fatal(err)
		}
		if _, published := details[key]; published {
			markReady(t, client, pod, key)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pod %s never published %s", pod, key)
}
