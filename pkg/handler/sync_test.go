//go:build !integration

package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// testCtxKey is a private context key type used by tests to avoid key collisions.
type testCtxKey struct{}

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
	if opts.WorkloadNamespace != "default" {
		t.Fatalf("expected namespace 'default', got %q", opts.WorkloadNamespace)
	}
}

func TestSyncOptions_SetContext(t *testing.T) {
	opts := &SyncOptions{}
	if opts.ctx != nil {
		t.Fatal("ctx should be nil before SetContext")
	}

	ctx := context.WithValue(context.Background(), testCtxKey{}, "test-value")
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

	if len(opts.getRollbackFuncs()) != 0 {
		t.Fatalf("expected 0 rollback funcs initially, got %d", len(opts.getRollbackFuncs()))
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

	funcs := opts.getRollbackFuncs()
	if len(funcs) != 3 {
		t.Fatalf("expected 3 rollback funcs, got %d", len(funcs))
	}

	// Execute all rollback functions and verify they are callable
	for _, f := range funcs {
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
