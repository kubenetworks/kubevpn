package action

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestUninstall_RemovesProxyAndRouteRBAC is the regression test for issue #797:
// kubevpn uninstall leaked the -route and -proxy Role/RoleBinding because the
// daemon-layer Uninstall only deleted the eight core resources named
// kubevpn-traffic-manager. It must remove the full set.
func TestUninstall_RemovesProxyAndRouteRBAC(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	core := config.ConfigMapPodTrafficManager
	// proxyRBACName / routeRBACName are unexported in pkg/handler; mirror the naming.
	proxy := core + "-proxy"
	route := core + "-route"

	cs := fake.NewSimpleClientset(
		// core resources
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: core, Namespace: ns}},
		// route-discovery RBAC (the leaked resources)
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: route, Namespace: ns}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: route, Namespace: ns}},
		// proxy-inject RBAC (the leaked resources)
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: proxy, Namespace: ns}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: proxy, Namespace: ns}},
	)

	if err := Uninstall(ctx, cs, ns); err != nil {
		t.Fatalf("Uninstall returned error: %v", err)
	}

	checks := []struct {
		group string
		get   func() error
	}{
		{"deployments", func() error { _, e := cs.AppsV1().Deployments(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"jobs", func() error { _, e := cs.BatchV1().Jobs(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"services", func() error { _, e := cs.CoreV1().Services(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"configmaps", func() error { _, e := cs.CoreV1().ConfigMaps(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"secrets", func() error { _, e := cs.CoreV1().Secrets(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"serviceaccounts", func() error { _, e := cs.CoreV1().ServiceAccounts(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"roles", func() error { _, e := cs.RbacV1().Roles(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"rolebindings", func() error { _, e := cs.RbacV1().RoleBindings(ns).Get(ctx, core, metav1.GetOptions{}); return e }},
		{"route roles", func() error { _, e := cs.RbacV1().Roles(ns).Get(ctx, route, metav1.GetOptions{}); return e }},
		{"route rolebindings", func() error { _, e := cs.RbacV1().RoleBindings(ns).Get(ctx, route, metav1.GetOptions{}); return e }},
		{"proxy roles", func() error { _, e := cs.RbacV1().Roles(ns).Get(ctx, proxy, metav1.GetOptions{}); return e }},
		{"proxy rolebindings", func() error { _, e := cs.RbacV1().RoleBindings(ns).Get(ctx, proxy, metav1.GetOptions{}); return e }},
	}
	for _, c := range checks {
		if err := c.get(); err == nil {
			t.Errorf("%s/%s still exists after uninstall", c.group, core)
		}
	}
}

// TestUninstall_RemovesCentralClusterScopedRBAC verifies that uninstalling the central
// kubevpn namespace also removes the cluster-scoped proxy-inject ClusterRole/
// ClusterRoleBinding (central mode).
func TestUninstall_RemovesCentralClusterScopedRBAC(t *testing.T) {
	ctx := context.Background()
	ns := config.DefaultNamespaceKubevpn
	core := config.ConfigMapPodTrafficManager
	proxy := core + "-proxy"

	cs := fake.NewSimpleClientset(
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: proxy}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: proxy}},
	)

	if err := Uninstall(ctx, cs, ns); err != nil {
		t.Fatalf("Uninstall returned error: %v", err)
	}

	if _, err := cs.RbacV1().ClusterRoles().Get(ctx, proxy, metav1.GetOptions{}); err == nil {
		t.Errorf("ClusterRole %s should be removed in central mode", proxy)
	}
	if _, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, proxy, metav1.GetOptions{}); err == nil {
		t.Errorf("ClusterRoleBinding %s should be removed in central mode", proxy)
	}
}

// TestUninstall_IdempotentOnEmptyNamespace ensures uninstall does not fail when the
// namespace has no traffic-manager resources at all (already clean / fresh cluster).
func TestUninstall_IdempotentOnEmptyNamespace(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	if err := Uninstall(ctx, cs, "fresh-ns"); err != nil {
		t.Fatalf("Uninstall on empty namespace returned error: %v", err)
	}
}
