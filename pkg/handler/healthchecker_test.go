package handler

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHealthStatus_ConfigMap_nil(t *testing.T) {
	var hs HealthStatus
	if hs.ConfigMap() != nil {
		t.Fatal("expected nil ConfigMap on zero-value HealthStatus")
	}
}

func TestHealthStatus_ConfigMap_set(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: "default"},
	}
	hs := HealthStatus{cm: cm}
	if hs.ConfigMap() != cm {
		t.Fatal("expected ConfigMap() to return the set ConfigMap")
	}
}

func TestHealthStatus_LastError_nil(t *testing.T) {
	var hs HealthStatus
	if hs.LastError() != nil {
		t.Fatal("expected nil LastError on zero-value HealthStatus")
	}
}

func TestHealthStatus_LastError_set(t *testing.T) {
	err := errors.New("connection refused")
	hs := HealthStatus{lastErr: err}
	if hs.LastError() != err {
		t.Fatal("expected LastError() to return the set error")
	}
}

func TestConnectOptions_HealthStatus(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-traffic-manager", Namespace: "test"},
	}
	err := errors.New("timeout")
	opts := ConnectOptions{
		healthStatus: HealthStatus{lastErr: err, cm: cm},
	}
	hs := opts.HealthStatus()
	if hs.LastError() != err {
		t.Fatal("expected HealthStatus().LastError() to match")
	}
	if hs.ConfigMap() != cm {
		t.Fatal("expected HealthStatus().ConfigMap() to match")
	}
}
