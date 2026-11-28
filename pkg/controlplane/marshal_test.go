package controlplane

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func fullVirtualV2() *Virtual {
	return &Virtual{
		SchemaVersion: CurrentSchemaVersion,
		Namespace:     "production",
		UID:           "services.reviews.nodes.node-a",
		Selector:      "app=reviews",
		ServiceName:   "reviews",
		AgentOrigins: []*AgentOrigin{{
			PodUID: "service/default/reviews", ServiceName: "reviews", PodIP: "10.96.0.10", OriginIP: "10.96.0.10",
			Ports: []AgentOriginPort{{ContainerPort: 8080, OriginPort: 80, Protocol: corev1.ProtocolTCP, AgentListenerPort: 20001}},
		}},
		Ports: []ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		Rules: []*Rule{{
			Headers: map[string]string{"version": "v2"}, LocalTunIPv4: "198.18.0.5", LocalTunIPv6: "2001:2::5",
			OwnerID: "a1b2c3d4e5f6", PortMap: map[int32]string{8080: "18080"}, IsAgent: true,
			SyncTarget: &SyncTarget{UID: "deployments.apps.reviews-sync-a", Selector: "owner-ref=reviews-sync-a"},
		}},
	}
}

func legacyVirtual() *Virtual {
	return &Virtual{
		Namespace: "default", UID: "deployments.apps.web",
		Ports: []ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		Rules: []*Rule{{Headers: map[string]string{"version": "v1"}, LocalTunIPv4: "10.0.0.1", OwnerID: "", PortMap: map[int32]string{8080: "9090"}}},
	}
}

func TestVirtualRoundTrip_CurrentAndLegacy(t *testing.T) {
	for _, original := range []*Virtual{fullVirtualV2(), legacyVirtual()} {
		data, err := yaml.Marshal([]*Virtual{original})
		if err != nil {
			t.Fatal(err)
		}
		var got []*Virtual
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !reflect.DeepEqual(original, got[0]) {
			t.Fatalf("round-trip mismatch\noriginal: %+v\ngot: %+v", original, got)
		}
	}
}

func TestVirtualSerializedKeyNames(t *testing.T) {
	data, err := yaml.Marshal([]*Virtual{fullVirtualV2()})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Uid", "Namespace", "Ports", "Rules", "ownerID", "Headers", "LocalTunIPv4", "LocalTunIPv6", "PortMap", "containerPort", "schemaVersion", "selector", "serviceName", "agentOrigins", "syncTarget"} {
		if !bytes.Contains(data, []byte(key+":")) {
			t.Errorf("serialized YAML missing %q:\n%s", key, data)
		}
	}
	var raw []map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"UID", "uid", "OwnerID", "fargateMode", "envoyListenerPort"} {
		if _, exists := raw[0][key]; exists {
			t.Errorf("serialized YAML contains removed or incorrect key %q:\n%s", key, data)
		}
	}
}

func TestVirtualSchemaVersion0OmittedFromOutput(t *testing.T) {
	data, err := yaml.Marshal([]*Virtual{legacyVirtual()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "schemaVersion:") {
		t.Fatalf("SchemaVersion=0 must be omitted:\n%s", data)
	}
}
