package handler

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	"testing"
	"time"

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

// TestConnectCreateOutboundPod_RBACLifecycle covers issue #805: reusing a
// central manager must not access RBAC with the connecting user's credentials.
func TestConnectCreateOutboundPod_RBACLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		managerNS  string
		existing   bool
		deployment bool
	}{
		{"existing central manager with restricted client", config.DefaultNamespaceKubevpn, true, true},
		{"new central manager", config.DefaultNamespaceKubevpn, false, false},
		{"central manager with lingering pod", config.DefaultNamespaceKubevpn, true, false},
		{"existing per-namespace manager", "team", true, true},
		{"new per-namespace manager", "team", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cs := fake.NewSimpleClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tc.managerNS}})
			if tc.deployment {
				if err := cs.Tracker().Add(genDeploySpec(tc.managerNS, "test-image", "")); err != nil {
					t.Fatal(err)
				}
			}
			podLists := 0
			cs.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				podLists++
				if !tc.existing && podLists == 1 {
					return true, &v1.PodList{}, nil
				}
				return true, &v1.PodList{Items: []v1.Pod{{
					ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: tc.managerNS, Labels: map[string]string{"app": config.ConfigMapPodTrafficManager}},
					Status: v1.PodStatus{
						Phase:             v1.PodRunning,
						Conditions:        []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
						ContainerStatuses: []v1.ContainerStatus{{Name: "vpn", Ready: true, State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}}},
					},
				}}}, nil
			})
			if tc.managerNS == config.DefaultNamespaceKubevpn && tc.deployment {
				cs.PrependReactor("*", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
					if action.GetResource().Group == "rbac.authorization.k8s.io" {
						t.Errorf("restricted client attempted RBAC action: %s %s", action.GetVerb(), action.GetResource().Resource)
						return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", fmt.Errorf("client cannot manage RBAC"))
					}
					return false, nil, nil
				})
			}
			c := &ConnectOptions{
				SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
				ManagerNamespace: tc.managerNS, WorkloadNamespace: "workloads", Image: "test-image",
			}
			if err := c.CreateOutboundPod(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.managerNS == config.DefaultNamespaceKubevpn && tc.deployment {
				return
			}
			// Creation/recreation and per-namespace reuse must still provision both
			// capabilities, binding the manager SA in the workload namespace.
			if _, err := cs.RbacV1().Roles("workloads").Get(ctx, routeRBACName, metav1.GetOptions{}); err != nil {
				t.Fatal(err)
			}
			routeBinding, err := cs.RbacV1().RoleBindings("workloads").Get(ctx, routeRBACName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(routeBinding.Subjects) != 1 || routeBinding.Subjects[0].Namespace != tc.managerNS {
				t.Fatalf("unexpected route binding subjects: %+v", routeBinding.Subjects)
			}
			if tc.managerNS == config.DefaultNamespaceKubevpn {
				if _, err := cs.RbacV1().ClusterRoles().Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := cs.RbacV1().Roles("workloads").Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := cs.RbacV1().RoleBindings("workloads").Get(ctx, proxyRBACName, metav1.GetOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
