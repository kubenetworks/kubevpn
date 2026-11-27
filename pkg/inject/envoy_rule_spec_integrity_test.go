package inject

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func TestEnvoyRuleSpec_NodeAgentIntegrity(t *testing.T) {
	const ns = "test-ns"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)
	spec := envoyRuleSpec{
		Namespace: ns, NodeID: "deployments.apps.myapp", LocalTunIPv4: "198.18.0.1", LocalTunIPv6: "fd00::1",
		Headers: map[string]string{"version": "v2"}, Ports: []controlplane.ContainerPort{{ContainerPort: 8080, Protocol: "TCP"}},
		PortMap: map[int32]string{8080: "18080"}, OwnerID: "owner-a", IsAgent: true, Selector: "app=myapp",
	}
	if err := addEnvoyConfig(context.Background(), mapInterface, spec); err != nil {
		t.Fatal(err)
	}
	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var virtuals []*controlplane.Virtual
	if err := yamlUnmarshal([]byte(updated.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatal(err)
	}
	if len(virtuals) != 1 || virtuals[0].UID != spec.NodeID || virtuals[0].Selector != spec.Selector || !virtuals[0].Rules[0].IsAgent {
		t.Fatalf("node-agent fields lost: %+v", virtuals)
	}
	rule := virtuals[0].Rules[0]
	if rule.OwnerID != spec.OwnerID || rule.LocalTunIPv4 != spec.LocalTunIPv4 || rule.LocalTunIPv6 != spec.LocalTunIPv6 || rule.PortMap[8080] != "18080" || rule.Headers["version"] != "v2" {
		t.Fatalf("rule fields lost: %+v", rule)
	}
}

func TestEnvoyRuleSpec_ValidateGuard(t *testing.T) {
	valid := envoyRuleSpec{Namespace: "test-ns", NodeID: "deployments.apps.nginx", LocalTunIPv4: "10.0.0.1", OwnerID: "owner-a"}
	tests := []struct {
		name      string
		mutate    func(*envoyRuleSpec)
		wantError bool
	}{
		{name: "valid"},
		{name: "missing namespace", mutate: func(s *envoyRuleSpec) { s.Namespace = "" }, wantError: true},
		{name: "missing node ID", mutate: func(s *envoyRuleSpec) { s.NodeID = "" }, wantError: true},
		{name: "missing local IP", mutate: func(s *envoyRuleSpec) { s.LocalTunIPv4 = "" }, wantError: true},
		{name: "missing owner ID", mutate: func(s *envoyRuleSpec) { s.OwnerID = "" }, wantError: true},
		{name: "valid sync target without user IP", mutate: func(s *envoyRuleSpec) {
			s.LocalTunIPv4 = ""
			s.SyncTarget = &controlplane.SyncTarget{UID: "deployments.apps.nginx-sync-a", Selector: "owner-ref=nginx-sync-a"}
		}},
		{name: "sync target missing selector", mutate: func(s *envoyRuleSpec) {
			s.LocalTunIPv4 = ""
			s.SyncTarget = &controlplane.SyncTarget{UID: "deployments.apps.nginx-sync-a"}
		}, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid
			if tt.mutate != nil {
				tt.mutate(&spec)
			}
			err := spec.validate()
			if tt.wantError && err == nil || !tt.wantError && err != nil {
				t.Fatalf("validate() error = %v, wantError=%v", err, tt.wantError)
			}
		})
	}
}
