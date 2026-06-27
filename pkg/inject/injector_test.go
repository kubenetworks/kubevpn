package inject

import (
	"encoding/json"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	if _, ok := injector.(*fargateInjector); !ok {
		t.Fatalf("expected *fargateInjector, got %T", injector)
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
	if _, ok := injector.(*meshInjector); !ok {
		t.Fatalf("expected *meshInjector, got %T", injector)
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
	if _, ok := injector.(*meshInjector); !ok {
		t.Fatalf("expected *meshInjector, got %T", injector)
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
	if _, ok := injector.(*meshInjector); !ok {
		t.Fatalf("expected *meshInjector (VPN-only = mesh with empty headers), got %T", injector)
	}
}

func Test_clearPodMetadata(t *testing.T) {
	now := metav1.Now()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-pod",
			Namespace:       "default",
			SelfLink:        "/api/v1/namespaces/default/pods/test-pod",
			Generation:      3,
			ResourceVersion: "12345",
			UID:             types.UID("abc-123"),
			DeletionTimestamp: &now,
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate},
			},
			OwnerReferences: []metav1.OwnerReference{
				{Name: "owner", UID: types.UID("owner-uid")},
			},
		},
	}

	clearPodMetadata(pod)

	if pod.SelfLink != "" {
		t.Errorf("expected SelfLink to be cleared, got %q", pod.SelfLink)
	}
	if pod.Generation != 0 {
		t.Errorf("expected Generation to be 0, got %d", pod.Generation)
	}
	if pod.ResourceVersion != "" {
		t.Errorf("expected ResourceVersion to be cleared, got %q", pod.ResourceVersion)
	}
	if pod.UID != "" {
		t.Errorf("expected UID to be cleared, got %q", pod.UID)
	}
	if pod.DeletionTimestamp != nil {
		t.Errorf("expected DeletionTimestamp to be nil, got %v", pod.DeletionTimestamp)
	}
	if pod.ManagedFields != nil {
		t.Errorf("expected ManagedFields to be nil, got %v", pod.ManagedFields)
	}
	if pod.OwnerReferences != nil {
		t.Errorf("expected OwnerReferences to be nil, got %v", pod.OwnerReferences)
	}
	// Name and Namespace should remain unchanged
	if pod.Name != "test-pod" {
		t.Errorf("expected Name to remain %q, got %q", "test-pod", pod.Name)
	}
	if pod.Namespace != "default" {
		t.Errorf("expected Namespace to remain %q, got %q", "default", pod.Namespace)
	}
}

func TestJSONPatchOp_Marshal(t *testing.T) {
	op := JSONPatchOp{
		Op:    "replace",
		Path:  "/spec/template/spec",
		Value: map[string]string{"key": "value"},
	}
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if decoded["op"] != "replace" {
		t.Errorf("expected op %q, got %q", "replace", decoded["op"])
	}
	if decoded["path"] != "/spec/template/spec" {
		t.Errorf("expected path %q, got %q", "/spec/template/spec", decoded["path"])
	}
	valMap, ok := decoded["value"].(map[string]any)
	if !ok {
		t.Fatalf("expected value to be a map, got %T", decoded["value"])
	}
	if valMap["key"] != "value" {
		t.Errorf("expected value.key %q, got %q", "value", valMap["key"])
	}

	// Verify omitempty: empty fields should be omitted
	empty := JSONPatchOp{}
	emptyData, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("unexpected marshal error for empty op: %v", err)
	}
	if string(emptyData) != "{}" {
		t.Errorf("expected empty op to marshal to %q, got %q", "{}", string(emptyData))
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
