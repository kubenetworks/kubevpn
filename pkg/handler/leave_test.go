package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes/fake"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

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
		proxyManager:     newProxyManager(nil, clientset, "test-ns"),
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
		proxyManager:     newProxyManager(nil, clientset, "test-ns"),
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

func TestProxyManager_Remove_RemovesWorkload(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/app1", namespace: "default"})
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/app2", namespace: "default"})
	c.proxyManager.Add(&Proxy{workload: "services/svc1", namespace: "other-ns"})

	c.proxyManager.Remove("default", "deployments.apps/app1")

	res := c.proxyManager.Resources()
	if len(res) != 2 {
		t.Fatalf("expected 2 remaining workloads, got %d", len(res))
	}
	for _, p := range res {
		if p.workload == "deployments.apps/app1" && p.namespace == "default" {
			t.Fatal("app1 should have been removed")
		}
	}
}

func TestProxyManager_Remove_NonExistentWorkload(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/app1", namespace: "default"})

	// Removing a non-existent workload should not panic or modify the list
	c.proxyManager.Remove("default", "deployments.apps/does-not-exist")

	res := c.proxyManager.Resources()
	if len(res) != 1 {
		t.Fatalf("expected 1 workload unchanged, got %d", len(res))
	}
}

func TestProxyManager_Remove_AllWorkloads(t *testing.T) {
	c := newTestConnectOptions(t)
	c.proxyManager = newProxyManager(c.factory, c.clientset, c.ManagerNamespace)
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/app1", namespace: "ns1"})
	c.proxyManager.Add(&Proxy{workload: "deployments.apps/app2", namespace: "ns2"})

	c.proxyManager.Remove("ns1", "deployments.apps/app1")
	c.proxyManager.Remove("ns2", "deployments.apps/app2")

	res := c.proxyManager.Resources()
	if len(res) != 0 {
		t.Fatalf("expected 0 workloads after removing all, got %d", len(res))
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

func TestLeaveAllProxyResources_WithProxyWorkloads(t *testing.T) {
	kubeconfig := "/nonexistent-kubeconfig-for-test"
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &kubeconfig
	factory := cmdutil.NewFactory(configFlags)

	clientset := fake.NewSimpleClientset(
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data:       map[string]string{config.KeyEnvoy: "[]"},
		},
	)
	pm := newProxyManager(factory, clientset, "test-ns")
	pm.Add(&Proxy{workload: "deployments.apps/web", namespace: "default"})
	pm.Add(&Proxy{workload: "deployments.apps/api", namespace: "default"})
	c := &ConnectOptions{
		SessionBase: SessionBase{K8sClient: K8sClient{
			clientset: clientset,
			factory:   factory,
		}},
		ManagerNamespace: "test-ns",
		proxyManager:     pm,
	}
	// proxyManager has entries, so LeaveAll will be called, which calls GetTopOwnerObject.
	// With a bad kubeconfig, GetTopOwnerObject returns an error for each workload.
	err := c.LeaveAllProxyResources(context.Background())
	if err == nil {
		t.Fatal("expected error when resources can't be resolved")
	}
}

func TestLeaveResource_ErrorPropagationFromGetTopOwnerObject(t *testing.T) {
	kubeconfig := "/nonexistent-kubeconfig-for-test"
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &kubeconfig
	factory := cmdutil.NewFactory(configFlags)

	clientset := fake.NewSimpleClientset()
	c := &ConnectOptions{
		SessionBase: SessionBase{K8sClient: K8sClient{
			clientset: clientset,
			factory:   factory,
		}},
		ManagerNamespace: "test-ns",
		proxyManager:     newProxyManager(factory, clientset, "test-ns"),
	}
	resources := []Resources{
		{Namespace: "default", Workload: "deployments.apps/nonexistent"},
	}
	err := c.LeaveResource(context.Background(), resources, "198.18.0.1")
	if err == nil {
		t.Fatal("expected error from GetTopOwnerObject, got nil")
	}
}

func TestLeaveResource_MultipleErrorsAggregated(t *testing.T) {
	kubeconfig := "/nonexistent-kubeconfig-for-test"
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.KubeConfig = &kubeconfig
	factory := cmdutil.NewFactory(configFlags)

	clientset := fake.NewSimpleClientset()
	c := &ConnectOptions{
		SessionBase: SessionBase{K8sClient: K8sClient{
			clientset: clientset,
			factory:   factory,
		}},
		ManagerNamespace: "test-ns",
		proxyManager:     newProxyManager(factory, clientset, "test-ns"),
	}
	resources := []Resources{
		{Namespace: "default", Workload: "deployments.apps/app1"},
		{Namespace: "default", Workload: "deployments.apps/app2"},
		{Namespace: "other-ns", Workload: "statefulsets.apps/db"},
	}
	err := c.LeaveResource(context.Background(), resources, "198.18.0.1")
	if err == nil {
		t.Fatal("expected aggregate error, got nil")
	}
}
