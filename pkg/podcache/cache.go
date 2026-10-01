// Package podcache shares node-local Pod observations across daemon requests.
// Cached observations are not proof of current API state or network readiness.
package podcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
)

// ErrNotReady means the cache has not completed its initial sync or has stopped.
var ErrNotReady = errors.New("pod cache is not ready: initial sync incomplete or cache stopped")

// Cache owns one informer for all Pods assigned to one node. It must be started
// once, not once per CNI request. Get never falls back to the API; a caller
// that needs an authoritative answer has to ask for one itself.
type Cache struct {
	informer cache.SharedIndexInformer
	lister   corelisters.PodLister
	nodeName string
	synced   chan struct{}
	done     chan struct{}

	mu      sync.Mutex
	started bool
	running bool
	runCtx  context.Context
}

// New constructs a cache without starting any API requests.
func New(client kubernetes.Interface, nodeName string) (*Cache, error) {
	if client == nil || strings.TrimSpace(nodeName) == "" {
		return nil, errors.New("pod cache requires a client and node name")
	}
	informer := coreinformers.NewFilteredPodInformer(client, metav1.NamespaceAll, 0,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		})
	if err := informer.SetTransform(trimPod); err != nil {
		return nil, fmt.Errorf("set Pod cache transform: %w", err)
	}
	c := &Cache{
		informer: informer, lister: corelisters.NewPodLister(informer.GetIndexer()), nodeName: nodeName,
		synced: make(chan struct{}), done: make(chan struct{}),
	}
	return c, nil
}

// Run blocks for the cache lifetime and joins the informer during cancellation.
// A stopped Cache cannot be restarted; construct a new one instead.
func (c *Cache) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("pod cache Run may only be called once")
	}
	c.started, c.running, c.runCtx = true, true, ctx
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.running = false
		close(c.done)
	}()
	informerDone := make(chan struct{})
	go func() {
		defer close(informerDone)
		c.informer.RunWithContext(ctx)
	}()
	if cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		close(c.synced)
	}
	<-informerDone
	return nil
}

// WaitForSync waits only for initial synchronization, not ongoing watch health
// or observation freshness. The caller bounds startup with its context.
func (c *Cache) WaitForSync(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-c.done:
		return false
	case <-c.synced:
		return ctx.Err() == nil && c.isReady()
	}
}

// Get returns a defensive copy containing only retained observation fields.
func (c *Cache) Get(namespace, name string) (*corev1.Pod, error) {
	if !c.isReady() {
		return nil, ErrNotReady
	}
	pod, err := c.lister.Pods(namespace).Get(name)
	if err != nil {
		return nil, err
	}
	// Keep the node boundary even if a client returns an object outside its filter.
	if pod.Spec.NodeName != c.nodeName {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	}
	return pod.DeepCopy(), nil
}

func (c *Cache) isReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readyLocked()
}

func (c *Cache) readyLocked() bool {
	return c.running && c.runCtx.Err() == nil && c.informer.HasSynced()
}
func trimPod(obj interface{}) (interface{}, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("unexpected Pod cache object %T", obj)
	}
	// Whether the Pod carries an annotations map at all is retained even when
	// no tracked key is in it. A writer builds its patch from this copy, and
	// the operation that creates a missing map replaces the whole map, so a
	// trimmed copy that understated an existing map would erase every
	// annotation the Pod has.
	var annotations map[string]string
	if pod.Annotations != nil {
		annotations = make(map[string]string)
		// Spelled once, in the package that owns them: a rename there must not
		// silently start stripping an annotation a reader depends on.
		for _, key := range []string{
			connectiondetails.NetworksAnnotation,
			connectiondetails.Annotation,
			connectiondetails.StatusAnnotation,
		} {
			if value, exists := pod.Annotations[key]; exists {
				annotations[key] = value
			}
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID, ResourceVersion: pod.ResourceVersion,
			DeletionTimestamp: pod.DeletionTimestamp.DeepCopy(), Annotations: annotations,
		},
		Spec: corev1.PodSpec{NodeName: pod.Spec.NodeName},
	}, nil
}
