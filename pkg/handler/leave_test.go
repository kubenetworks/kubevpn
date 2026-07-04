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
		ManagerNamespace: "test-ns",
		clientset:        clientset,
		proxyWorkloads:   ProxyList{},
	}
	err := c.LeaveAllProxyResources(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for empty envoy key, got: %v", err)
	}
}

func TestLeaveAllProxyResources_ConfigMapNotFound(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	c := &ConnectOptions{
		ManagerNamespace: "test-ns",
		clientset:        clientset,
		proxyWorkloads:   ProxyList{},
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

func TestLeavePortMap_RemovesWorkload(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{
		{workload: "deployments.apps/app1", namespace: "default"},
		{workload: "deployments.apps/app2", namespace: "default"},
		{workload: "services/svc1", namespace: "other-ns"},
	}

	c.leavePortMap("default", "deployments.apps/app1")

	if len(c.proxyWorkloads) != 2 {
		t.Fatalf("expected 2 remaining workloads, got %d", len(c.proxyWorkloads))
	}
	for _, p := range c.proxyWorkloads {
		if p.workload == "deployments.apps/app1" && p.namespace == "default" {
			t.Fatal("app1 should have been removed")
		}
	}
}

func TestLeavePortMap_NonExistentWorkload(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{
		{workload: "deployments.apps/app1", namespace: "default"},
	}

	// Removing a non-existent workload should not panic or modify the list
	c.leavePortMap("default", "deployments.apps/does-not-exist")

	if len(c.proxyWorkloads) != 1 {
		t.Fatalf("expected 1 workload unchanged, got %d", len(c.proxyWorkloads))
	}
}

func TestLeavePortMap_AllWorkloads(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyWorkloads = ProxyList{
		{workload: "deployments.apps/app1", namespace: "ns1"},
		{workload: "deployments.apps/app2", namespace: "ns2"},
	}

	c.leavePortMap("ns1", "deployments.apps/app1")
	c.leavePortMap("ns2", "deployments.apps/app2")

	if len(c.proxyWorkloads) != 0 {
		t.Fatalf("expected 0 workloads after removing all, got %d", len(c.proxyWorkloads))
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
			Data:       map[string]string{config.KeyDHCP: "", config.KeyDHCP6: "", config.KeyEnvoy: ""},
		},
	)

	c := &ConnectOptions{
		ManagerNamespace: "test-ns",
		clientset:        clientset,
		ctx:              ctx,
		cancel:           cancel,
	}

	// Cleanup as root daemon (ctx != nil) with no rollback funcs — should not panic
	c.Cleanup(context.Background())
}

func TestCleanup_WithRollbackFuncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyDHCP: "", config.KeyDHCP6: "", config.KeyEnvoy: ""},
		},
	)

	var called []int
	c := &ConnectOptions{
		ManagerNamespace: "test-ns",
		clientset:        clientset,
		ctx:              ctx,
		cancel:           cancel,
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

func TestCleanup_UserDaemon_WithRollbackFuncs(t *testing.T) {
	// User daemon: ctx is nil
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyDHCP: "", config.KeyDHCP6: "", config.KeyEnvoy: ""},
		},
	)

	var called []int
	c := &ConnectOptions{
		ManagerNamespace: "test-ns",
		clientset:        clientset,
		// ctx is nil => userDaemon = true
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
	c := &ConnectOptions{
		ManagerNamespace: "test-ns",
		clientset:        clientset,
		ctx:              ctx,
		cancel:           cancel,
	}
	c.AddRollbackFunc(func() error { callCount++; return nil })

	c.Cleanup(context.Background())
	c.Cleanup(context.Background())
	c.Cleanup(context.Background())

	if callCount != 1 {
		t.Fatalf("expected rollback called once due to sync.Once, got %d", callCount)
	}
}
