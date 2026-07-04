package inject

import (
	"testing"

	v1 "k8s.io/api/core/v1"
)

func TestGatherContainerPorts_FromSpec(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
						{ContainerPort: 9090, Protocol: v1.ProtocolTCP},
					},
				},
				{
					Name: "sidecar",
					Ports: []v1.ContainerPort{
						{ContainerPort: 3000, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	ports := gatherContainerPorts(spec, nil)
	if len(ports) != 3 {
		t.Fatalf("expected 3 ports, got %d", len(ports))
	}
}

func TestGatherContainerPorts_WithPortMaps(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	// Add a port that doesn't exist in spec
	ports := gatherContainerPorts(spec, []string{"9090:9090"})
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports (1 from spec + 1 from portMap), got %d", len(ports))
	}
}

func TestGatherContainerPorts_DeduplicatesExisting(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	// Port 8080 already exists in spec — should not be added again
	ports := gatherContainerPorts(spec, []string{"8080:8080"})
	if len(ports) != 1 {
		t.Fatalf("expected 1 port (dedup), got %d", len(ports))
	}
}

func TestCollectPorts_MeshMode(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
						{ContainerPort: 9090, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	envoyPorts, portmap := collectPorts(spec, nil)
	if len(envoyPorts) != 2 {
		t.Fatalf("expected 2 envoy ports, got %d", len(envoyPorts))
	}
	if len(portmap) != 2 {
		t.Fatalf("expected 2 portmap entries, got %d", len(portmap))
	}
	// Default portmap: containerPort → "containerPort"
	if portmap[8080] != "8080" {
		t.Fatalf("portmap[8080]: want '8080', got %q", portmap[8080])
	}
}

func TestCollectPorts_WithHostPortOverride(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	envoyPorts, portmap := collectPorts(spec, []string{"8080:19080"})
	if len(envoyPorts) != 1 {
		t.Fatalf("expected 1 port, got %d", len(envoyPorts))
	}
	// portMap should override with hostPort
	if portmap[8080] != "19080" {
		t.Fatalf("portmap[8080]: want '19080', got %q", portmap[8080])
	}
}

func TestCollectFargatePorts(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
						{ContainerPort: 9090, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	ports, portmap := collectFargatePorts(spec, nil)
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(ports))
	}
	if len(portmap) != 2 {
		t.Fatalf("expected 2 portmap entries, got %d", len(portmap))
	}

	// Each entry should be "envoyRulePort:localPort"
	for containerPort, pair := range portmap {
		if pair == "" {
			t.Fatalf("empty portmap for %d", containerPort)
		}
		// Should contain a colon
		if len(pair) < 3 {
			t.Fatalf("portmap[%d] too short: %q", containerPort, pair)
		}
		t.Logf("portmap[%d] = %s", containerPort, pair)
	}
}

func TestCollectFargatePorts_WithHostPortOverride(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	_, portmap := collectFargatePorts(spec, []string{"8080:19080"})

	// localPort should be overridden to 19080
	pair := portmap[8080]
	// Format: "randomEnvoyRulePort:19080"
	parts := splitPortPair(pair)
	if parts[1] != "19080" {
		t.Fatalf("expected localPort=19080, got %s in pair %q", parts[1], pair)
	}
}

func splitPortPair(pair string) [2]string {
	for i, c := range pair {
		if c == ':' {
			return [2]string{pair[:i], pair[i+1:]}
		}
	}
	return [2]string{pair, ""}
}

func TestAlreadyInjected(t *testing.T) {
	spec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app"},
				{Name: "vpn"},
				{Name: "envoy-proxy"},
			},
		},
	}
	if !alreadyInjected(spec) {
		t.Fatal("expected already injected (has 'vpn' + 'envoy-proxy' containers)")
	}

	spec2 := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{Name: "app"},
			},
		},
	}
	if alreadyInjected(spec2) {
		t.Fatal("expected not injected")
	}
}

func TestRemoveContainers(t *testing.T) {
	spec := &v1.PodSpec{
		Containers: []v1.Container{
			{Name: "app"},
			{Name: "vpn"},
			{Name: "envoy-proxy"},
			{Name: "helper"},
		},
	}
	RemoveContainers(spec)
	if len(spec.Containers) != 2 {
		t.Fatalf("expected 2 containers after removal, got %d", len(spec.Containers))
	}
	for _, c := range spec.Containers {
		if c.Name == "vpn" || c.Name == "envoy-proxy" {
			t.Fatalf("sidecar %s should have been removed", c.Name)
		}
	}
}

func TestRenderEnvoyConfig(t *testing.T) {
	tmpl := "address: {{.}}"
	result := RenderEnvoyConfig(tmpl, "kubevpn-traffic-manager.default")
	if result != "address: kubevpn-traffic-manager.default" {
		t.Fatalf("unexpected render: %q", result)
	}
}

func TestRenderEnvoyConfig_Invalid(t *testing.T) {
	result := RenderEnvoyConfig("{{.Invalid", "value")
	if result != "" {
		t.Fatalf("expected empty for invalid template, got %q", result)
	}
}
