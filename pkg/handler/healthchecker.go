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

func (c *ConnectOptions) Health(ctx context.Context) {
	for ctx.Err() == nil {
		go func() {
			timeoutCtx, cancelFunc := context.WithTimeout(ctx, time.Second*5)
			defer cancelFunc()

			mapInterface := c.GetClientset().CoreV1().ConfigMaps(c.Namespace)
			configMap, err := mapInterface.Get(timeoutCtx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
			if err != nil {
				c.healthStatus.lastErr = err
				return
			}
			c.healthStatus.lastErr = nil
			c.healthStatus.cm = configMap
		}()
		time.Sleep(time.Second * 5)
	}
}

func (c *ConnectOptions) HealthStatus() HealthStatus {
	return c.healthStatus
}
