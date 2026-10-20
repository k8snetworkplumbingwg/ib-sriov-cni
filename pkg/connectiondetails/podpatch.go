package connectiondetails

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/observability"
)

// DefaultKubeconfig is generated on the host by the ib-sriov-cni DaemonSet
// entrypoint from its own service account (multus pattern).
const DefaultKubeconfig = "/etc/cni/net.d/ib-sriov-cni.d/ib-sriov-cni.kubeconfig"

// publishTimeout bounds one API phase of a publication attempt: the identity
// GET, or the merge/PATCH loop. The PF rebind between the two phases waits for
// the CNI lock and takes seconds per delegate, so it is charged to neither.
const publishTimeout = 10 * time.Second

// PublishContext derives the budget for one API phase from parent.
func PublishContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, publishTimeout)
}

// PodClient is the minimal pod-annotation access the publisher needs.
type PodClient struct {
	clientset kubernetes.Interface
}

// NewPodClient builds a PodClient from a kubeconfig path (DefaultKubeconfig when
// empty), normalizing a bracketed-IPv4 apiserver host that client-go rejects.
func NewPodClient(kubeconfig string) (*PodClient, error) {
	if kubeconfig == "" {
		kubeconfig = DefaultKubeconfig
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig %s: %w", kubeconfig, err)
	}
	cfg.Host = NormalizeAPIServerHost(cfg.Host)
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	return &PodClient{clientset: clientset}, nil
}

// NewPodClientForClientset adapts a clientset the caller already built, so a
// daemon publishes through the same connection pool its informer watches on.
func NewPodClientForClientset(clientset kubernetes.Interface) *PodClient {
	return &PodClient{clientset: clientset}
}

// NormalizeAPIServerHost strips brackets from bracketed IPv4 literals
// ("https://[10.96.0.1]:443" -> "https://10.96.0.1:443"): Go's url parser
// rejects bracketed IPv4 outright, so this is plain string surgery. Some
// Multus-style kubeconfig generators emit that form and Multus tolerates it.
// Bracketed IPv6 literals are left untouched; brackets are required there.
func NormalizeAPIServerHost(host string) string {
	i := strings.Index(host, "://")
	if i < 0 {
		return host
	}
	rest := host[i+3:]
	if !strings.HasPrefix(rest, "[") {
		return host
	}
	end := strings.Index(rest, "]")
	if end < 0 {
		return host
	}
	inner := rest[1:end]
	ip := net.ParseIP(inner)
	if ip == nil || ip.To4() == nil {
		return host
	}
	return host[:i+3] + inner + rest[end+1:]
}

// GetPod returns one coherent snapshot of identity, network selections, current
// connection details, and resourceVersion for a publication attempt.
func (c *PodClient) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	observability.LivePodGet(ctx, observability.LiveGetReason(ctx), err)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s/%s: %w", namespace, name, err)
	}
	return pod, nil
}

// GetPodAuthoritative is GetPod: this client always reads the API server. It
// exists so a caller can demand a read it can act on without knowing whether
// the client it was handed sits in front of a cache.
func (c *PodClient) GetPodAuthoritative(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.GetPod(ctx, namespace, name)
}

// PatchAnnotation sets the connection-details annotation with a JSON Patch
// (RFC 6902) whose `test` on resourceVersion makes the write fail if another
// writer changed the pod since it was read - so a concurrent CNI ADD for a
// second interface can't silently overwrite the first entry; the caller
// retries. When the pod has no annotations map yet, the whole map is added
// instead of a key under it (RFC 6902 `add` requires the parent to exist).
func (c *PodClient) PatchAnnotation(ctx context.Context, namespace, name, value, resourceVersion string, annotationsPresent bool) error {
	ops := []map[string]interface{}{
		{"op": "test", "path": "/metadata/resourceVersion", "value": resourceVersion},
	}
	if annotationsPresent {
		ops = append(ops, map[string]interface{}{
			"op": "add", "path": "/metadata/annotations/" + jsonPointerEscape(Annotation), "value": value,
		})
	} else {
		ops = append(ops, map[string]interface{}{
			"op": "add", "path": "/metadata/annotations", "value": map[string]string{Annotation: value},
		})
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("failed to marshal annotation patch: %w", err)
	}
	if _, err := c.clientset.CoreV1().Pods(namespace).Patch(
		ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("failed to patch pod %s/%s annotation: %w", namespace, name, err)
	}
	return nil
}

// jsonPointerEscape escapes a JSON Pointer reference token (RFC 6901):
// '~' -> '~0' and '/' -> '~1', in that order.
func jsonPointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
