package xds

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// writeConfigMapKeyIfAbsent fills key with value only when it is currently empty
// (after TrimSpace), using an optimistic read-modify-write under RetryOnConflict.
// It is the shared cache-warm-up primitive for the traffic-manager ConfigMap: both
// the cluster-CIDR cache (tun_config_cidr.go) and the cluster-DNS cache
// (tun_config_dns.go) are "fill an empty key, never overwrite, best-effort" — the
// same Get→skip-if-present→RMW loop — so they share this helper instead of
// triplicating the RetryOnConflict boilerplate.
//
// Returns nil on success (including the "already populated, skipped" path) or the
// underlying Get/Update error. Callers log the error and fall back to the
// pre-existing behavior (empty cache → client-side detection); they do not propagate
// it, so a cache write failure never fails the manager.
func (s *TunConfigServer) writeConfigMapKeyIfAbsent(ctx context.Context, key, value string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if strings.TrimSpace(cm.Data[key]) != "" {
			return nil // filled in the meantime — leave it (additive: never overwrite)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[key] = value
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// configMapKeyAbsent reports whether the given key is unset/empty on the traffic-manager
// ConfigMap, used by the warm-up pre-check that avoids the RMW entirely when the cache
// is already populated. Trims whitespace so a stray newline does not block a real fill.
func (s *TunConfigServer) configMapKeyAbsent(ctx context.Context, key string) (bool, error) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(cm.Data[key]) == "", nil
}
