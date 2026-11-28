package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestLeaveAllProxyResources_NilConnectOptions(t *testing.T) {
	var c *ConnectOptions
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for nil ConnectOptions, got: %v", err)
	}
}

func TestLeaveAllProxyResources_NilClientset(t *testing.T) {
	c := &ConnectOptions{}
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for nil clientset, got: %v", err)
	}
}

func TestLeaveAllProxyResources_EmptyConfigMap(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyEnvoy: ""},
		},
	)
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
	}
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for empty envoy key, got: %v", err)
	}
}

func TestLeaveAllProxyResources_ConfigMapNotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
	}
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("expected nil error when ConfigMap not found, got: %v", err)
	}
}

func TestLeaveResource_EmptyResources(t *testing.T) {
	c := newTestConnectOptions(t)
	err := c.LeaveResource(context.Background(), []Resources{}, "198.18.0.1")
	if err != nil {
		t.Fatalf("expected nil error for empty resources slice, got: %v", err)
	}
}

func TestLeaveResource_NilResources(t *testing.T) {
	c := newTestConnectOptions(t)
	err := c.LeaveResource(context.Background(), nil, "198.18.0.1")
	if err != nil {
		t.Fatalf("expected nil error for nil resources slice, got: %v", err)
	}
}

func TestCleanup_NilConnectOptions(t *testing.T) {
	var c *ConnectOptions
	// Should not panic
	c.Cleanup(context.Background())
}

func TestCleanup_NoRollbackFuncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
	)

	// Test data-plane cleanup path: use DataSession with a real cancel func.
	ds := &DataSession{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
		ctx:              ctx,
		cancel:           cancel,
	}

	// Cleanup as root daemon with no rollback funcs — should not panic.
	ds.Cleanup(context.Background())
}

func TestCleanup_WithRollbackFuncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
	)

	var called []int
	// Test data-plane cleanup path with rollback funcs: use DataSession.
	ds := &DataSession{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
		ctx:              ctx,
		cancel:           cancel,
	}
	ds.AddRollbackFunc(func() error { called = append(called, 1); return nil })
	ds.AddRollbackFunc(func() error { called = append(called, 2); return nil })

	ds.Cleanup(context.Background())

	if len(called) != 2 {
		t.Fatalf("expected 2 rollback funcs called, got %d", len(called))
	}
	if called[0] != 1 || called[1] != 2 {
		t.Fatalf("expected rollback order [1, 2], got %v", called)
	}
}

func TestCleanup_UserDaemon_WithRollbackFuncs(t *testing.T) {
	// User daemon (ControlSession/ConnectOptions): cleanupControlPlane path.
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
	)

	var called []int
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
	}
	c.AddRollbackFunc(func() error { called = append(called, 1); return nil })
	c.AddRollbackFunc(func() error { called = append(called, 2); return nil })

	c.Cleanup(context.Background())

	if len(called) != 2 {
		t.Fatalf("expected 2 rollback funcs called, got %d", len(called))
	}
	if called[0] != 1 || called[1] != 2 {
		t.Fatalf("expected rollback order [1, 2], got %v", called)
	}
}

func TestCleanup_OnlyRunsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyEnvoy: ""},
		},
	)

	callCount := 0
	// Test idempotency using DataSession (data-plane path).
	ds := &DataSession{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
		ctx:              ctx,
		cancel:           cancel,
	}
	ds.AddRollbackFunc(func() error { callCount++; return nil })

	ds.Cleanup(context.Background())
	ds.Cleanup(context.Background())
	ds.Cleanup(context.Background())

	if callCount != 1 {
		t.Fatalf("expected rollback called once due to cleanedUp flag, got %d", callCount)
	}
}

func TestLeaveResource_ErrorsWhenManagerUnreachable(t *testing.T) {
	// Pure server-side leave: with no traffic manager Service to dial, LeaveResource
	// returns an error (no local fallback).
	clientset := fake.NewSimpleClientset()
	c := &ConnectOptions{
		SessionBase:      SessionBase{K8sClient: K8sClient{clientset: clientset}},
		ManagerNamespace: "test-ns",
	}
	resources := []Resources{
		{Namespace: "default", Workload: "deployments.apps/nonexistent"},
	}
	if err := c.LeaveResource(context.Background(), resources, "198.18.0.1"); err == nil {
		t.Fatal("expected error when manager is unreachable, got nil")
	}
}
