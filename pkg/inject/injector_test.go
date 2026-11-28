package inject

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

func TestNewInjector_Service(t *testing.T) {
	opts := InjectOptions{
		Object: &resource.Info{
			Mapping: &meta.RESTMapping{
				Resource: schema.GroupVersionResource{Resource: "services"},
			},
		},
	}
	injector := NewInjector(opts)
	if _, ok := injector.(*nodeAgentInjector); !ok {
		t.Fatalf("expected *nodeAgentInjector, got %T", injector)
	}
}

func TestNewInjector_WithHeaders(t *testing.T) {
	opts := InjectOptions{
		Object: &resource.Info{
			Mapping: &meta.RESTMapping{
				Resource: schema.GroupVersionResource{Resource: "deployments"},
			},
		},
		Headers: map[string]string{"x-test": "value"},
	}
	injector := NewInjector(opts)
	if _, ok := injector.(*nodeAgentInjector); !ok {
		t.Fatalf("expected *nodeAgentInjector, got %T", injector)
	}
}

func TestNewInjector_WithPortMaps(t *testing.T) {
	opts := InjectOptions{
		Object: &resource.Info{
			Mapping: &meta.RESTMapping{
				Resource: schema.GroupVersionResource{Resource: "deployments"},
			},
		},
		PortMaps: []string{"8080:80"},
	}
	injector := NewInjector(opts)
	if _, ok := injector.(*nodeAgentInjector); !ok {
		t.Fatalf("expected *nodeAgentInjector, got %T", injector)
	}
}

func TestNewInjector_NoHeadersNoPortMaps(t *testing.T) {
	opts := InjectOptions{
		Object: &resource.Info{
			Mapping: &meta.RESTMapping{
				Resource: schema.GroupVersionResource{Resource: "deployments"},
			},
		},
	}
	injector := NewInjector(opts)
	if _, ok := injector.(*nodeAgentInjector); !ok {
		t.Fatalf("expected *nodeAgentInjector (VPN-only = mesh with empty headers), got %T", injector)
	}
}

func TestGatherContainerPorts_MultipleContainers(t *testing.T) {
	templateSpec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "web",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP},
						{ContainerPort: 443, Protocol: v1.ProtocolTCP},
					},
				},
				{
					Name: "api",
					Ports: []v1.ContainerPort{
						{ContainerPort: 8080, Protocol: v1.ProtocolTCP}, // overlapping with web
						{ContainerPort: 9090, Protocol: v1.ProtocolTCP},
					},
				},
				{
					Name: "sidecar",
					Ports: []v1.ContainerPort{
						{ContainerPort: 443, Protocol: v1.ProtocolTCP}, // overlapping with web
						{ContainerPort: 3000, Protocol: v1.ProtocolTCP},
					},
				},
			},
		},
	}

	// portMaps includes one duplicate (8080) and one new (5432)
	portMaps := []string{"8080:80", "5432:5432"}
	ports := gatherContainerPorts(templateSpec, portMaps)

	// All 6 container ports should be gathered, plus 5432 (8080 already known)
	expectedCount := 7 // 2 + 2 + 2 from containers + 1 new from portMaps
	if len(ports) != expectedCount {
		t.Fatalf("expected %d ports, got %d: %+v", expectedCount, len(ports), ports)
	}

	// Verify 5432 is present (added from portMaps)
	found5432 := false
	for _, p := range ports {
		if p.ContainerPort == 5432 {
			found5432 = true
			// HostPort should be zeroed
			if p.HostPort != 0 {
				t.Errorf("expected HostPort 0 for portMap-added port 5432, got %d", p.HostPort)
			}
		}
	}
	if !found5432 {
		t.Error("expected port 5432 from portMaps to be added, but not found")
	}

	// Verify duplicates from containers are preserved (not deduped among containers)
	count8080 := 0
	for _, p := range ports {
		if p.ContainerPort == 8080 {
			count8080++
		}
	}
	if count8080 != 2 {
		t.Errorf("expected 2 occurrences of port 8080 from containers, got %d", count8080)
	}
}

func TestCollectPorts_EmptySpec(t *testing.T) {
	templateSpec := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			Containers: []v1.Container{},
		},
	}

	envoyPorts, portmap := collectPorts(templateSpec, nil)

	if len(envoyPorts) != 0 {
		t.Errorf("expected 0 envoy ports for empty spec, got %d", len(envoyPorts))
	}
	if len(portmap) != 0 {
		t.Errorf("expected empty portmap for empty spec, got %d entries", len(portmap))
	}
}
