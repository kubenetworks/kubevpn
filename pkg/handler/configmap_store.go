package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
// storage and health monitoring. It owns a shared informer and health check state.
type ConfigMapStore struct {
	clientset        kubernetes.Interface
	managerNamespace string

	informerOnce sync.Once
	informer     cache.SharedInformer
	informerStop chan struct{}

	healthStatus HealthStatus
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
		s.informerStop = make(chan struct{})
		go s.informer.Run(s.informerStop)
	})
	return s.informer
}

// Get retrieves a value by key from the traffic manager ConfigMap, using the informer cache first.
func (s *ConfigMapStore) Get(ctx context.Context, key string) (string, error) {
	items := s.GetInformer().GetStore().List()
	for _, item := range items {
		if cm, ok := item.(*corev1.ConfigMap); ok {
			return cm.Data[key], nil
		}
	}
	cm, err := s.clientset.CoreV1().ConfigMaps(s.managerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return cm.Data[key], nil
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

// HealthPeriod periodically syncs the traffic manager ConfigMap into healthStatus.
// Reads from the shared informer cache (zero API calls when cache is warm),
// with a direct GET fallback to detect API server unreachable.
func (s *ConfigMapStore) HealthPeriod(ctx context.Context, _ time.Duration) {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.syncFromCache()
			s.HealthCheckOnce(ctx, time.Second*10)
		case <-ctx.Done():
			return
		}
	}
}

// syncFromCache updates healthStatus from the shared informer's local cache.
func (s *ConfigMapStore) syncFromCache() {
	items := s.GetInformer().GetStore().List()
	for _, item := range items {
		if cm, ok := item.(*corev1.ConfigMap); ok {
			s.healthStatus.lastErr = nil
			s.healthStatus.cm = cm
			return
		}
	}
}

// HealthCheckOnce performs a single health check with the given timeout.
func (s *ConfigMapStore) HealthCheckOnce(ctx context.Context, timeout time.Duration) {
	timeoutCtx, cancelFunc := context.WithTimeout(ctx, timeout)
	defer cancelFunc()

	mapInterface := s.clientset.CoreV1().ConfigMaps(s.managerNamespace)
	configMap, err := mapInterface.Get(timeoutCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		s.healthStatus.lastErr = err
		plog.G(ctx).Debugf("Health check failed: %v", err)
		return
	}
	s.healthStatus.lastErr = nil
	s.healthStatus.cm = configMap
}

// GetHealthStatus returns the last known health state.
func (s *ConfigMapStore) GetHealthStatus() HealthStatus {
	return s.healthStatus
}

// Stop closes the informer stop channel, shutting down the shared informer.
func (s *ConfigMapStore) Stop() {
	if s.informerStop != nil {
		close(s.informerStop)
	}
}
