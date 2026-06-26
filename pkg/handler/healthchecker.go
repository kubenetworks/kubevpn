package handler

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// HealthStatus tracks the last known state of the traffic manager ConfigMap.
type HealthStatus struct {
	lastErr error
	cm      *corev1.ConfigMap
}

func (h *HealthStatus) ConfigMap() *corev1.ConfigMap {
	return h.cm
}

func (h *HealthStatus) LastError() error {
	return h.lastErr
}

// HealthPeriod periodically syncs the traffic manager ConfigMap into healthStatus.
// Reads from the shared informer cache (zero API calls when cache is warm),
// with a direct GET fallback to detect API server unreachable.
func (c *ConnectOptions) HealthPeriod(ctx context.Context, _ time.Duration) {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.syncFromCache()
			c.HealthCheckOnce(ctx, time.Second*10)
		case <-ctx.Done():
			return
		}
	}
}

// syncFromCache updates healthStatus from the shared informer's local cache.
func (c *ConnectOptions) syncFromCache() {
	items := c.GetConfigMapInformer().GetStore().List()
	for _, item := range items {
		if cm, ok := item.(*corev1.ConfigMap); ok {
			c.healthStatus.lastErr = nil
			c.healthStatus.cm = cm
			return
		}
	}
}

func (c *ConnectOptions) HealthCheckOnce(ctx context.Context, timeout time.Duration) {
	timeoutCtx, cancelFunc := context.WithTimeout(ctx, timeout)
	defer cancelFunc()

	mapInterface := c.GetClientset().CoreV1().ConfigMaps(c.ManagerNamespace)
	configMap, err := mapInterface.Get(timeoutCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		c.healthStatus.lastErr = err
		plog.G(ctx).Debugf("Health check failed: %v", err)
		return
	}
	c.healthStatus.lastErr = nil
	c.healthStatus.cm = configMap
}

func (c *ConnectOptions) HealthStatus() HealthStatus {
	return c.healthStatus
}
