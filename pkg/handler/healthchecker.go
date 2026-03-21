package handler

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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

func (c *ConnectOptions) HealthPeriod(ctx context.Context, duration time.Duration) {
	for ctx.Err() == nil {
		time.Sleep(duration)
		c.HealthCheckOnce(ctx, duration)
	}
}

func (c *ConnectOptions) HealthCheckOnce(ctx context.Context, timeout time.Duration) {
	timeoutCtx, cancelFunc := context.WithTimeout(ctx, timeout)
	defer cancelFunc()

	mapInterface := c.GetClientset().CoreV1().ConfigMaps(c.Namespace)
	configMap, err := mapInterface.Get(timeoutCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		c.healthStatus.lastErr = err
		return
	}
	c.healthStatus.lastErr = nil
	c.healthStatus.cm = configMap
}

func (c *ConnectOptions) HealthStatus() HealthStatus {
	return c.healthStatus
}
