package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	k8stypes "k8s.io/apimachinery/pkg/types"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ConfigMapStore provides access to the traffic manager ConfigMap for key-value
// storage, backed by a shared informer cache. Reads are cache-only (no live-API
// fallback): callers warm the cache once via EnsureSynced at connection establishment,
// after which every read is served from memory and a cache miss returns empty instead
// of a doomed live GET that would block on the TCP timeout when the cluster is
// unreachable. Writes (Set) still go straight to the API.
type ConfigMapStore struct {
	clientset        kubernetes.Interface
	managerNamespace string

	informerOnce sync.Once
	informer     cache.SharedInformer
	informerStop chan struct{}

	stopMu  sync.Mutex
	stopped bool
}

// newConfigMapStore creates a new ConfigMapStore for the given clientset and namespace.
func newConfigMapStore(clientset kubernetes.Interface, managerNamespace string) *ConfigMapStore {
	return &ConfigMapStore{
		clientset:        clientset,
		managerNamespace: managerNamespace,
	}
}

// GetInformer returns a shared informer for the traffic manager ConfigMap.
// Created once on first call, then reused. Thread-safe via sync.Once.
func (s *ConfigMapStore) GetInformer() cache.SharedInformer {
	s.informerOnce.Do(func() {
		s.informer = informerv1.NewFilteredConfigMapInformer(
			s.clientset, s.managerNamespace, 0, cache.Indexers{},
			func(options *metav1.ListOptions) {
				options.FieldSelector = fields.OneTermEqualSelector("metadata.name", config.ConfigMapPodTrafficManager).String()
			},
		)
		stop := make(chan struct{})
		// Publish informerStop under stopMu so a concurrent Stop() (which reads it
		// under the same lock) has a happens-before relationship with this write.
		s.stopMu.Lock()
		s.informerStop = stop
		s.stopMu.Unlock()
		go s.informer.Run(stop)
	})
	return s.informer
}

// EnsureSynced starts the informer (if not already running) and blocks until its cache has
// completed the initial List, bounded by config.ConfigMapSyncTimeout. Callers invoke this
// once at connection establishment so that all later reads are served from the warm cache.
//
// It returns an error when the cache cannot sync within the bound. The store has no live-API
// read fallback, so a connection whose cache never warms would read "absent" forever; connect
// callers therefore treat this error as fatal (fail fast). By this point the traffic manager
// ConfigMap already exists, so a sync failure means the API is unreachable — which the rest of
// connect would fail on anyway.
func (s *ConfigMapStore) EnsureSynced(ctx context.Context) error {
	informer := s.GetInformer()
	ctx, cancel := context.WithTimeout(ctx, config.ConfigMapSyncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("timed out waiting for traffic manager ConfigMap cache to sync")
	}
	return nil
}

// Get retrieves a value by key from the traffic manager ConfigMap via the informer cache.
// The cache is warmed by EnsureSynced at connect time; a cache miss returns "" (not an
// error), which callers treat as "not set". There is NO live-API fallback — a doomed live
// GET to an unreachable cluster used to make every daemon read block on the TCP timeout.
func (s *ConfigMapStore) Get(_ context.Context, key string) (string, error) {
	for _, item := range s.GetInformer().GetStore().List() {
		if cm, ok := item.(*corev1.ConfigMap); ok {
			return cm.Data[key], nil
		}
	}
	return "", nil
}

// GetConfigMap returns the traffic manager ConfigMap from the warm informer cache (see
// EnsureSynced). A cache miss returns (nil, nil) — callers render it as "no proxy state"
// and fall back to heartbeat-based status instead of blocking on a live-API GET.
func (s *ConfigMapStore) GetConfigMap(_ context.Context) (*corev1.ConfigMap, error) {
	for _, item := range s.GetInformer().GetStore().List() {
		if cm, ok := item.(*corev1.ConfigMap); ok {
			return cm, nil
		}
	}
	return nil, nil
}

// Set updates a key-value pair in the traffic manager ConfigMap.
func (s *ConfigMapStore) Set(ctx context.Context, key, value string) error {
	err := retry.RetryOnConflict(
		retry.DefaultRetry,
		func() error {
			patch := []map[string]string{{
				"op":    "replace",
				"path":  "/data/" + key,
				"value": value,
			}}
			p, err := json.Marshal(patch)
			if err != nil {
				return fmt.Errorf("failed to marshal JSON patch: %w", err)
			}
			_, err = s.clientset.CoreV1().ConfigMaps(s.managerNamespace).Patch(ctx, config.ConfigMapPodTrafficManager, k8stypes.JSONPatchType, p, metav1.PatchOptions{})
			return err
		})
	if err != nil {
		plog.G(ctx).Errorf("Failed to update configmap: %v", err)
		return err
	}
	return nil
}

// Stop closes the informer stop channel, shutting down the shared informer.
// It is idempotent: repeated calls (e.g. from a retried Cleanup) are safe and
// never close the channel twice.
func (s *ConfigMapStore) Stop() {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.informerStop != nil && !s.stopped {
		close(s.informerStop)
		s.stopped = true
	}
}
