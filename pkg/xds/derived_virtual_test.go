package xds

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestWorkloadsOwnedBy_UsesDesiredState(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data: map[string]string{config.KeyEnvoy: `- Namespace: default
  Uid: deployments.apps.reviews
  Rules:
  - ownerID: owner
- Namespace: default
  Uid: deployments.apps.details
  Rules:
  - ownerID: another-owner
- Namespace: other
  Uid: deployments.apps.reviews
  Rules:
  - ownerID: owner
`},
	}
	server := &TunConfigServer{clientset: fake.NewSimpleClientset(cm), namespace: "kubevpn"}
	workloads, err := server.workloadsOwnedBy(context.Background(), "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deployments.apps/reviews"}
	if !reflect.DeepEqual(workloads, want) {
		t.Fatalf("leave-all selected invalid desired workloads: got %v want %v", workloads, want)
	}
}
