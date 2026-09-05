package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestManagerDial_FailsWithoutClusterIP verifies that, with injection now server-side
// only (no local fallback), a missing / headless / empty-ClusterIP traffic manager
// Service makes the inject and leave paths return an error rather than silently
// succeeding. No cluster required.
func TestManagerDial_FailsWithoutClusterIP(t *testing.T) {
	cases := []struct {
		name string
		svc  *v1.Service
	}{
		{name: "no manager service"},
		{name: "headless manager service", svc: &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
			Spec:       v1.ServiceSpec{ClusterIP: v1.ClusterIPNone},
		}},
		{name: "empty clusterIP", svc: &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
			Spec:       v1.ServiceSpec{ClusterIP: ""},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cs *fake.Clientset
			if tc.svc != nil {
				cs = fake.NewSimpleClientset(tc.svc)
			} else {
				cs = fake.NewSimpleClientset()
			}
			c := &ConnectOptions{
				SessionBase:      SessionBase{K8sClient: K8sClient{clientset: cs}},
				ManagerNamespace: "kubevpn",
			}
			if err := c.createRemoteInboundViaManager(context.Background(), "kubevpn", nil, nil, "img", "198.18.0.5", "", []string{"deployments.apps/foo"}); err == nil {
				t.Fatal("inject: expected error when manager is unreachable, got nil")
			}
			if err := c.leaveViaManager(context.Background(), "kubevpn", []string{"deployments.apps/foo"}, "owner-a"); err == nil {
				t.Fatal("leave: expected error when manager is unreachable, got nil")
			}
		})
	}
}
