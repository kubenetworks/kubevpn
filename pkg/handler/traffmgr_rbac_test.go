package handler

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestEnsureProxyRBAC_Scope verifies the proxy-inject RBAC scope depends on the manager
// namespace: a central manager (config.DefaultNamespaceKubevpn) gets a cluster-scoped
// ClusterRole/ClusterRoleBinding; a per-namespace manager gets a namespaced Role/RoleBinding.
func TestEnsureProxyRBAC_Scope(t *testing.T) {
	ctx := context.Background()

	t.Run("central manager -> ClusterRole", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		ensureProxyRBAC(ctx, cs, "default", config.DefaultNamespaceKubevpn)

		if _, err := cs.RbacV1().ClusterRoles().Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
			t.Fatalf("expected ClusterRole %q, got err: %v", proxyRBACName, err)
		}
		if _, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
			t.Fatalf("expected ClusterRoleBinding %q, got err: %v", proxyRBACName, err)
		}
		if _, err := cs.RbacV1().Roles("default").Get(ctx, proxyRBACName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected no namespaced Role for a central manager, got err: %v", err)
		}
	})

	t.Run("per-namespace manager -> Role", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		ensureProxyRBAC(ctx, cs, "myns", "myns")

		if _, err := cs.RbacV1().Roles("myns").Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
			t.Fatalf("expected Role %q in myns, got err: %v", proxyRBACName, err)
		}
		if _, err := cs.RbacV1().RoleBindings("myns").Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
			t.Fatalf("expected RoleBinding %q in myns, got err: %v", proxyRBACName, err)
		}
		if _, err := cs.RbacV1().ClusterRoles().Get(ctx, proxyRBACName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected no ClusterRole for a per-namespace manager, got err: %v", err)
		}
	})
}
