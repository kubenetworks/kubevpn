//go:build !integration

package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

func TestSyncOptions_InitClient(t *testing.T) {
	// Create a minimal but valid kubeconfig file pointing to a dummy server.
	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "kubeconfig")
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: default
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: fake-token
`
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0600); err != nil {
		t.Fatalf("failed to write temp kubeconfig: %v", err)
	}

	f := util.InitFactoryByPath(kubeconfigPath, "default")
	opts := &SyncOptions{}
	err := opts.InitClient(f)
	if err != nil {
		t.Fatalf("InitClient returned error: %v", err)
	}

	if opts.GetFactory() == nil {
		t.Fatal("factory is nil after InitClient")
	}
	if opts.config == nil {
		t.Fatal("config is nil after InitClient")
	}
	if opts.GetClientset() == nil {
		t.Fatal("clientset is nil after InitClient")
	}
	if opts.restclient == nil {
		t.Fatal("restclient is nil after InitClient")
	}
	if opts.Namespace != "default" {
		t.Fatalf("expected namespace 'default', got %q", opts.Namespace)
	}
}

func TestSyncOptions_SetContext(t *testing.T) {
	opts := &SyncOptions{}
	if opts.ctx != nil {
		t.Fatal("ctx should be nil before SetContext")
	}

	ctx := context.WithValue(context.Background(), struct{}{}, "test-value")
	opts.SetContext(ctx)

	if opts.ctx == nil {
		t.Fatal("ctx is nil after SetContext")
	}
	if opts.ctx != ctx {
		t.Fatal("SetContext did not store the provided context")
	}
}

func TestSyncOptions_AddRollbackFunc(t *testing.T) {
	opts := &SyncOptions{}

	if len(opts.rollbackFuncList) != 0 {
		t.Fatalf("expected 0 rollback funcs initially, got %d", len(opts.rollbackFuncList))
	}

	var callOrder []int
	opts.AddRollbackFunc(func() error {
		callOrder = append(callOrder, 1)
		return nil
	})
	opts.AddRollbackFunc(func() error {
		callOrder = append(callOrder, 2)
		return errors.New("rollback error")
	})
	opts.AddRollbackFunc(func() error {
		callOrder = append(callOrder, 3)
		return nil
	})

	if len(opts.rollbackFuncList) != 3 {
		t.Fatalf("expected 3 rollback funcs, got %d", len(opts.rollbackFuncList))
	}

	// Execute all rollback functions and verify they are callable
	for _, f := range opts.rollbackFuncList {
		_ = f()
	}

	if len(callOrder) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(callOrder))
	}
	if callOrder[0] != 1 || callOrder[1] != 2 || callOrder[2] != 3 {
		t.Fatalf("unexpected call order: %v", callOrder)
	}
}

func TestSyncOptions_GetSyncthingGUIAddr(t *testing.T) {
	opts := &SyncOptions{}
	addr := opts.GetSyncthingGUIAddr()
	if addr != "" {
		t.Fatalf("expected empty syncthing GUI addr, got %q", addr)
	}
}

func TestSyncOptions_GetFactory(t *testing.T) {
	opts := &SyncOptions{}
	if opts.GetFactory() != nil {
		t.Fatal("expected nil factory before InitClient")
	}

	tmpDir := t.TempDir()
	kubeconfigPath := filepath.Join(tmpDir, "kubeconfig")
	kubeconfigContent := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    namespace: test-ns
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: fake-token
`
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfigContent), 0600); err != nil {
		t.Fatalf("failed to write temp kubeconfig: %v", err)
	}

	f := util.InitFactoryByPath(kubeconfigPath, "test-ns")
	if err := opts.InitClient(f); err != nil {
		t.Fatalf("InitClient: %v", err)
	}
	if opts.GetFactory() == nil {
		t.Fatal("expected non-nil factory after InitClient")
	}
}

func TestHealthStatus_ConfigMap(t *testing.T) {
	hs := HealthStatus{}
	if hs.ConfigMap() != nil {
		t.Fatal("expected nil ConfigMap initially")
	}

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-configmap",
			Namespace: "test-ns",
		},
		Data: map[string]string{"key": "value"},
	}
	hs.cm = cm

	got := hs.ConfigMap()
	if got == nil {
		t.Fatal("ConfigMap() returned nil after setting")
	}
	if got.Name != "test-configmap" {
		t.Fatalf("expected name 'test-configmap', got %q", got.Name)
	}
	if got.Namespace != "test-ns" {
		t.Fatalf("expected namespace 'test-ns', got %q", got.Namespace)
	}
	if got.Data["key"] != "value" {
		t.Fatalf("expected data key='value', got %q", got.Data["key"])
	}
}

func TestHealthStatus_LastError(t *testing.T) {
	hs := HealthStatus{}
	if hs.LastError() != nil {
		t.Fatal("expected nil error initially")
	}

	expectedErr := errors.New("connection refused")
	hs.lastErr = expectedErr

	got := hs.LastError()
	if got == nil {
		t.Fatal("LastError() returned nil after setting")
	}
	if got.Error() != "connection refused" {
		t.Fatalf("expected 'connection refused', got %q", got.Error())
	}
	if !errors.Is(got, expectedErr) {
		t.Fatal("returned error is not the same instance")
	}

	// Clear the error
	hs.lastErr = nil
	if hs.LastError() != nil {
		t.Fatal("expected nil after clearing error")
	}
}
