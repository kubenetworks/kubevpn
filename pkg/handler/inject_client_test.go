package handler

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// TestCreateRemoteInboundViaManager_FallsBack verifies the client-fallback trigger:
// when the traffic manager Service has no usable ClusterIP (or does not exist), the
// server-side injection attempt reports handled=false with no error, so
// CreateRemoteInboundPod proceeds with local injection. No cluster required.
func TestCreateRemoteInboundViaManager_FallsBack(t *testing.T) {
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
			handled, err := c.createRemoteInboundViaManager(context.Background(), "kubevpn", nil, nil, "img", "198.18.0.5", "", []string{"deployments.apps/foo"})
			if handled || err != nil {
				t.Fatalf("inject: expected fallback (handled=false, err=nil), got handled=%v err=%v", handled, err)
			}

			// leaveViaManager shares dialManager, so it falls back the same way.
			handled, err = c.leaveViaManager(context.Background(), "kubevpn", []string{"deployments.apps/foo"}, "owner-a")
			if handled || err != nil {
				t.Fatalf("leave: expected fallback (handled=false, err=nil), got handled=%v err=%v", handled, err)
			}
		})
	}
}
