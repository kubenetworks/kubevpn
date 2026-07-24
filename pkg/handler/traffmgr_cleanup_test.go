package handler

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestDeleteTrafficManagerCoreResources_RemovesAllEightResources verifies the shared
// teardown helper deletes every one of the eight namespaced resources that make up a
// traffic-manager instance, when they exist. This is the contract both
// cleanupTrafficManagerResources (handler) and Uninstall (daemon/action) rely on, so it
// is pinned here rather than in either caller.
func TestDeleteTrafficManagerCoreResources_RemovesAllEightResources(t *testing.T) {
	ctx := context.Background()
	ns := "default"
	name := config.ConfigMapPodTrafficManager

	cs := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
	)

	DeleteTrafficManagerCoreResources(ctx, cs, ns, name, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})

	checks := []struct {
		group string
		get   func() error
	}{
		{"deployments", func() error { _, e := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"jobs", func() error { _, e := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"services", func() error { _, e := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"configmaps", func() error { _, e := cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"secrets", func() error { _, e := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"serviceaccounts", func() error { _, e := cs.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"roles", func() error { _, e := cs.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
		{"rolebindings", func() error { _, e := cs.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{}); return e }},
	}
	for _, c := range checks {
		if err := c.get(); err == nil {
			t.Errorf("%s/%s still exists after teardown", c.group, name)
		}
	}
}

// TestDeleteTrafficManagerCoreResources_IdempotentOnMissing verifies the helper is
// best-effort: deleting resources that do not exist (fresh namespace) does not surface
// an error. Teardown runs on unreachable/already-partially-cleaned clusters, so a
// missing resource must not abort the rest of the deletion.
func TestDeleteTrafficManagerCoreResources_IdempotentOnMissing(t *testing.T) {
	ctx := context.Background()
	ns := "fresh-ns"
	cs := fake.NewSimpleClientset() // nothing exists

	// Must not panic or return an error — there is simply nothing to delete.
	DeleteTrafficManagerCoreResources(ctx, cs, ns, config.ConfigMapPodTrafficManager, metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)})

	// Verify the namespaced resources are indeed absent (sanity, not the core assertion).
	if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{}); err == nil {
		t.Error("configmap should not exist in a fresh namespace")
	}
}
