package xds

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func TestNewTunConfigServerRejectsMixedDesiredState(t *testing.T) {
	mixed := []*controlplane.Virtual{{
		Namespace: "default", UID: "deployments.apps.reviews.nodes.node-a",
		AgentOrigins: []*controlplane.AgentOrigin{{PodUID: "pod-a"}},
	}}
	data, err := yaml.Marshal(mixed)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
		Data:       map[string]string{config.KeyEnvoy: string(data), config.KeyTunIPPool: ""},
	}
	clientset := fake.NewSimpleClientset(cm)
	server, err := NewTunConfigServer(context.Background(), clientset, "test-ns")
	if server != nil {
		server.Close()
		t.Fatal("mixed desired state unexpectedly started a server")
	}
	if !errors.Is(err, controlplane.ErrMixedVirtualState) {
		t.Fatalf("expected mixed-state rejection, got %v", err)
	}
	stored, getErr := clientset.CoreV1().ConfigMaps("test-ns").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Data[config.KeyEnvoy] != string(data) {
		t.Fatal("mixed-state validation rewrote the old config")
	}
}
