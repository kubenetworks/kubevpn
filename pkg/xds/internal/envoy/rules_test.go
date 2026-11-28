package envoy

import (
	"context"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func TestRulesSync_DoesNotOverwriteSyncTarget(t *testing.T) {
	ctx := context.Background()
	virtuals := []*controlplane.Virtual{{
		Namespace: "default", UID: "deployments.apps.reviews",
		Rules: []*controlplane.Rule{
			{OwnerID: "owner-a", LocalTunIPv4: "198.18.0.1"},
			{OwnerID: "owner-a", SyncTarget: &controlplane.SyncTarget{UID: "deployments.apps.reviews-sync-a", Selector: "app=reviews-sync-a"}},
		},
	}}
	data, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"}, Data: map[string]string{config.KeyEnvoy: string(data)}}
	clientset := fake.NewSimpleClientset(cm)
	New(clientset, "kubevpn").Sync(ctx, "owner-a", &net.IPNet{IP: net.ParseIP("198.18.0.50"), Mask: net.CIDRMask(32, 32)}, nil)

	got, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	virtuals = nil
	if err := yaml.Unmarshal([]byte(got.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatal(err)
	}
	if got := virtuals[0].Rules[0].LocalTunIPv4; got != "198.18.0.50" {
		t.Fatalf("ordinary rule endpoint = %q, want DHCP update", got)
	}
	if got := virtuals[0].Rules[1].LocalTunIPv4; got != "" {
		t.Fatalf("sync target was overwritten with user TUN IP %q", got)
	}
}
