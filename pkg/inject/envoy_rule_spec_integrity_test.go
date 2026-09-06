package inject

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

// TestEnvoyRuleSpec_MeshIntegrity verifies that a fully-populated mesh-mode spec survives
// the full addEnvoyConfig → ConfigMap → unmarshal round-trip with all fields intact.
func TestEnvoyRuleSpec_MeshIntegrity(t *testing.T) {
	const ns = "test-ns"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)

	ports := []controlplane.ContainerPort{
		{ContainerPort: 8080, Protocol: "TCP"},
		{ContainerPort: 9090, Protocol: "TCP"},
	}
	headers := map[string]string{"version": "v2", "env": "staging"}
	portmap := map[int32]string{8080: "10080", 9090: "10090"}

	spec := envoyRuleSpec{
		Namespace:    ns,
		NodeID:       "deployments.apps.myapp",
		LocalTunIPv4: "192.168.100.1",
		LocalTunIPv6: "fd00::100",
		Headers:      headers,
		Ports:        ports,
		PortMap:      portmap,
		FargateMode:  false,
		OwnerID:      "owner-mesh-001",
	}

	if err := addEnvoyConfig(context.Background(), mapInterface, spec); err != nil {
		t.Fatalf("addEnvoyConfig (mesh) returned error: %v", err)
	}

	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	var virtuals []*controlplane.Virtual
	if err := yamlUnmarshal([]byte(updated.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("failed to unmarshal envoy config: %v", err)
	}

	if len(virtuals) != 1 {
		t.Fatalf("expected 1 Virtual, got %d", len(virtuals))
	}
	vr := virtuals[0]

	// Virtual-level assertions
	if vr.UID != spec.NodeID {
		t.Errorf("UID: got %q, want %q", vr.UID, spec.NodeID)
	}
	if vr.Namespace != ns {
		t.Errorf("Namespace: got %q, want %q", vr.Namespace, ns)
	}
	if vr.FargateMode {
		t.Error("FargateMode: got true, want false")
	}
	if vr.IsFargateMode() {
		t.Error("IsFargateMode(): got true, want false")
	}
	if len(vr.Ports) != 2 {
		t.Errorf("Ports: got %d, want 2", len(vr.Ports))
	}

	// Rule-level assertions
	if len(vr.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(vr.Rules))
	}
	rule := vr.Rules[0]

	if rule.OwnerID != spec.OwnerID {
		t.Errorf("OwnerID: got %q, want %q", rule.OwnerID, spec.OwnerID)
	}
	if rule.LocalTunIPv4 != spec.LocalTunIPv4 {
		t.Errorf("LocalTunIPv4: got %q, want %q", rule.LocalTunIPv4, spec.LocalTunIPv4)
	}
	if rule.LocalTunIPv6 != spec.LocalTunIPv6 {
		t.Errorf("LocalTunIPv6: got %q, want %q", rule.LocalTunIPv6, spec.LocalTunIPv6)
	}
	for k, want := range headers {
		if got := rule.Headers[k]; got != want {
			t.Errorf("Headers[%q]: got %q, want %q", k, got, want)
		}
	}
	if rule.PortMap == nil {
		t.Error("PortMap: got nil, want non-nil")
	}
	for port, want := range portmap {
		if got := rule.PortMap[port]; got != want {
			t.Errorf("PortMap[%d]: got %q, want %q", port, got, want)
		}
	}
}

// TestEnvoyRuleSpec_FargateIntegrity verifies that a fully-populated fargate-mode spec
// survives the full addEnvoyConfig → ConfigMap → unmarshal round-trip with all fields intact,
// including EnvoyListenerPort values on each port.
func TestEnvoyRuleSpec_FargateIntegrity(t *testing.T) {
	const ns = "fargate-ns"
	clientset := fake.NewSimpleClientset(newTestConfigMap(ns))
	mapInterface := clientset.CoreV1().ConfigMaps(ns)

	ports := []controlplane.ContainerPort{
		{ContainerPort: 8080, EnvoyListenerPort: 15001, Protocol: "TCP"},
		{ContainerPort: 9090, EnvoyListenerPort: 15002, Protocol: "TCP"},
	}
	headers := map[string]string{"user": "alice"}
	portmap := map[int32]string{8080: "15001:10080", 9090: "15002:10090"}

	spec := envoyRuleSpec{
		Namespace:    ns,
		NodeID:       "deployments.apps.webservice",
		LocalTunIPv4: "127.0.0.1",
		LocalTunIPv6: "::1",
		Headers:      headers,
		Ports:        ports,
		PortMap:      portmap,
		FargateMode:  true,
		OwnerID:      "owner-fargate-001",
	}

	if err := addEnvoyConfig(context.Background(), mapInterface, spec); err != nil {
		t.Fatalf("addEnvoyConfig (fargate) returned error: %v", err)
	}

	updated, err := mapInterface.Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	var virtuals []*controlplane.Virtual
	if err := yamlUnmarshal([]byte(updated.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("failed to unmarshal envoy config: %v", err)
	}

	if len(virtuals) != 1 {
		t.Fatalf("expected 1 Virtual, got %d", len(virtuals))
	}
	vr := virtuals[0]

	if !vr.FargateMode {
		t.Error("FargateMode: got false, want true")
	}
	if !vr.IsFargateMode() {
		t.Error("IsFargateMode(): got false, want true")
	}
	if len(vr.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(vr.Rules))
	}
	if vr.Rules[0].LocalTunIPv4 != "127.0.0.1" {
		t.Errorf("LocalTunIPv4: got %q, want 127.0.0.1", vr.Rules[0].LocalTunIPv4)
	}

	// Verify EnvoyListenerPort is preserved for each port
	portByContainer := make(map[int32]controlplane.ContainerPort)
	for _, p := range vr.Ports {
		portByContainer[p.ContainerPort] = p
	}
	expectedListenerPorts := map[int32]int32{8080: 15001, 9090: 15002}
	for containerPort, wantListener := range expectedListenerPorts {
		p, ok := portByContainer[containerPort]
		if !ok {
			t.Errorf("port %d not found in Virtual.Ports", containerPort)
			continue
		}
		if p.EnvoyListenerPort != wantListener {
			t.Errorf("port %d: EnvoyListenerPort got %d, want %d", containerPort, p.EnvoyListenerPort, wantListener)
		}
	}
}

// TestEnvoyRuleSpec_ValidateGuard is a table-driven test asserting that validate() rejects
// incomplete or inconsistent specs, and accepts complete ones.
func TestEnvoyRuleSpec_ValidateGuard(t *testing.T) {
	// completeMeshSpec is a fully valid mesh spec — validate() must accept it.
	completeMeshSpec := envoyRuleSpec{
		Namespace:    "test-ns",
		NodeID:       "deployments.apps.nginx",
		LocalTunIPv4: "10.0.0.1",
		LocalTunIPv6: "fd00::1",
		Headers:      map[string]string{"version": "v1"},
		Ports: []controlplane.ContainerPort{
			{ContainerPort: 8080, Protocol: "TCP"},
		},
		PortMap:     map[int32]string{8080: "9090"},
		FargateMode: false,
		OwnerID:     "owner-001",
	}

	// completeFargateSpec is a fully valid fargate spec — validate() must accept it.
	completeFargateSpec := envoyRuleSpec{
		Namespace:    "test-ns",
		NodeID:       "deployments.apps.web",
		LocalTunIPv4: "127.0.0.1",
		LocalTunIPv6: "::1",
		Headers:      map[string]string{"user": "alice"},
		Ports: []controlplane.ContainerPort{
			{ContainerPort: 8080, EnvoyListenerPort: 15001, Protocol: "TCP"},
		},
		PortMap:     map[int32]string{8080: "15001:9090"},
		FargateMode: true,
		OwnerID:     "owner-002",
	}

	// fargateNoEnvoyListenerPort has FargateMode=true but a port with EnvoyListenerPort=0.
	fargateNoEnvoyListenerPort := func() envoyRuleSpec {
		s := completeFargateSpec
		s.Ports = []controlplane.ContainerPort{
			{ContainerPort: 8080, EnvoyListenerPort: 0, Protocol: "TCP"},
		}
		return s
	}()

	// fargateEmptyPorts has FargateMode=true but zero ports — vacuously true, no error.
	fargateEmptyPorts := func() envoyRuleSpec {
		s := completeFargateSpec
		s.Ports = nil
		return s
	}()

	cases := []struct {
		name      string
		spec      envoyRuleSpec
		wantError bool
	}{
		{
			name:      "valid mesh spec",
			spec:      completeMeshSpec,
			wantError: false,
		},
		{
			name:      "valid fargate spec",
			spec:      completeFargateSpec,
			wantError: false,
		},
		{
			name: "missing namespace",
			spec: func() envoyRuleSpec {
				s := completeMeshSpec
				s.Namespace = ""
				return s
			}(),
			wantError: true,
		},
		{
			name: "missing nodeID",
			spec: func() envoyRuleSpec {
				s := completeMeshSpec
				s.NodeID = ""
				return s
			}(),
			wantError: true,
		},
		{
			name: "missing localTunIPv4",
			spec: func() envoyRuleSpec {
				s := completeMeshSpec
				s.LocalTunIPv4 = ""
				return s
			}(),
			wantError: true,
		},
		{
			name: "missing ownerID",
			spec: func() envoyRuleSpec {
				s := completeMeshSpec
				s.OwnerID = ""
				return s
			}(),
			wantError: true,
		},
		{
			name:      "fargate mode with port missing EnvoyListenerPort",
			spec:      fargateNoEnvoyListenerPort,
			wantError: true,
		},
		{
			name:      "fargate mode with empty ports (vacuously valid)",
			spec:      fargateEmptyPorts,
			wantError: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.validate()
			if tc.wantError && err == nil {
				t.Errorf("validate(): expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("validate(): expected no error, got %v", err)
			}
		})
	}
}
