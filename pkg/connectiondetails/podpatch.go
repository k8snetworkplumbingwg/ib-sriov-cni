package connectiondetails

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultKubeconfig is generated on the host by the ib-sriov-cni DaemonSet
// entrypoint from its own service account (multus pattern).
const DefaultKubeconfig = "/etc/cni/net.d/ib-sriov-cni.d/ib-sriov-cni.kubeconfig"

// publishTimeout is the single budget shared across the pod GET + PATCH of one
// best-effort publication attempt, so the added CNI-ADD latency can't
// accumulate multiple independent per-request timeouts.
const publishTimeout = 10 * time.Second

// PublishContext returns the shared deadline for one publication attempt. The
// caller passes the returned context to GetAnnotation and PatchAnnotation so
// the whole attempt is bounded by one budget.
func PublishContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), publishTimeout)
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
	// Some Multus-style kubeconfig generators write the apiserver as a bracketed
	// literal even for IPv4 ("https://[10.96.0.1]:443"); Multus tolerates it but
	// client-go rejects it. Unbracket IPv4 hosts.
	cfg.Host = normalizeAPIServerHost(cfg.Host)
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	return &PodClient{clientset: clientset}, nil
}

// normalizeAPIServerHost strips brackets from bracketed IPv4 literals
// ("https://[10.96.0.1]:443" -> "https://10.96.0.1:443"): Go's url parser
// rejects bracketed IPv4 outright, so this is plain string surgery.
// Bracketed IPv6 literals are left untouched; brackets are required there.
func normalizeAPIServerHost(host string) string {
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

// GetAnnotation returns the current value of the connection-details
// annotation on the pod ("" if unset).
func (c *PodClient) GetAnnotation(ctx context.Context, namespace, name string) (
	value, resourceVersion string, annotationsPresent bool, err error,
) {
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", false, fmt.Errorf("failed to get pod %s/%s: %w", namespace, name, err)
	}
	return pod.Annotations[Annotation], pod.ResourceVersion, pod.Annotations != nil, nil
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
