package handler

import (
	corev1 "k8s.io/api/core/v1"
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
