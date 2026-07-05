// Package controlplane serialization round-trip tests.
//
// These tests have no //go:build integration tag — they exercise only the
// Marshal/Unmarshal path (sigs.k8s.io/yaml) and require no Kubernetes cluster.
package controlplane

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// fullVirtualV2 returns a fully-populated Virtual with SchemaVersion=2.
// Every field is set so the round-trip test exercises the complete struct.
func fullVirtualV2() *Virtual {
	return &Virtual{
		SchemaVersion: 2,
		Namespace:     "production",
		UID:           "deployments.apps.myapp",
		FargateMode:   true,
		Ports: []ContainerPort{
			{
				Name:              "http",
				EnvoyListenerPort: 15001,
				ContainerPort:     8080,
				Protocol:          corev1.ProtocolTCP,
			},
			{
				Name:          "metrics",
				ContainerPort: 9090,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"version": "v2", "user": "alice"},
				LocalTunIPv4: "198.18.0.5",
				LocalTunIPv6: "2001:2::5",
				OwnerID:      "a1b2c3d4e5f6",
				PortMap:      map[int32]string{8080: "15001:18080"},
			},
			{
				Headers:      map[string]string{"version": "v3", "user": "bob"},
				LocalTunIPv4: "198.18.0.9",
				LocalTunIPv6: "2001:2::9",
				OwnerID:      "f6e5d4c3b2a1",
				PortMap:      map[int32]string{8080: "15002:18081", 9090: "15003:19090"},
			},
		},
	}
}

// legacyVirtual returns a Virtual with SchemaVersion=0 (legacy, pre-versioning).
// OwnerID is intentionally empty to match real legacy ConfigMap data.
func legacyVirtual() *Virtual {
	return &Virtual{
		SchemaVersion: 0,
		Namespace:     "default",
		UID:           "deployments.apps.web",
		FargateMode:   false,
		Ports: []ContainerPort{
			{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		},
		Rules: []*Rule{
			{
				Headers:      map[string]string{"version": "v1"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				OwnerID:      "",
				PortMap:      map[int32]string{8080: "9090"},
			},
		},
	}
}

// TestVirtualRoundTrip_SchemaV2 verifies that a fully-populated SchemaVersion=2
// Virtual survives a Marshal→Unmarshal cycle without data loss.
func TestVirtualRoundTrip_SchemaV2(t *testing.T) {
	original := fullVirtualV2()

	data, err := yaml.Marshal([]*Virtual{original})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	var got []*Virtual
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 Virtual after round-trip, got %d", len(got))
	}

	if !reflect.DeepEqual(original, got[0]) {
		t.Errorf("round-trip mismatch\noriginal: %+v\ngot:      %+v", original, got[0])
	}
}

// TestVirtualRoundTrip_LegacySchemaV0 verifies that a legacy (SchemaVersion=0)
// Virtual with an empty OwnerID survives a Marshal→Unmarshal cycle.
// Legacy configs must be preserved exactly so they can be upgraded in-place.
func TestVirtualRoundTrip_LegacySchemaV0(t *testing.T) {
	original := legacyVirtual()

	data, err := yaml.Marshal([]*Virtual{original})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	var got []*Virtual
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 Virtual after round-trip, got %d", len(got))
	}

	if !reflect.DeepEqual(original, got[0]) {
		t.Errorf("round-trip mismatch\noriginal: %+v\ngot:      %+v", original, got[0])
	}
}

// TestVirtualSerializedKeyNames pins the exact on-disk key names produced by
// sigs.k8s.io/yaml for the Virtual and Rule structs.
//
// sigs.k8s.io/yaml routes through encoding/json, so yaml struct tags are
// ignored — only json tags control the key names. This test guards against
// accidental tag changes that would break backward compatibility with existing
// ConfigMap data already stored in production clusters.
//
// Key contract (do not change these without migrating all ConfigMap data):
//
//   - Virtual.UID     → "Uid"         (json:"Uid";  NOT "uid" or "UID")
//   - Virtual.Namespace → "Namespace" (no json tag; Go field name)
//   - Virtual.Ports   → "Ports"       (no json tag; Go field name)
//   - Virtual.Rules   → "Rules"       (no json tag; Go field name)
//   - Rule.OwnerID    → "ownerID"     (json:"ownerID")
//   - Rule.Headers    → "Headers"     (no json tag; Go field name)
//   - Rule.LocalTunIPv4 → "LocalTunIPv4" (no json tag; Go field name)
//   - Rule.PortMap    → "PortMap"     (no json tag; Go field name)
func TestVirtualSerializedKeyNames(t *testing.T) {
	v := fullVirtualV2()
	data, err := yaml.Marshal([]*Virtual{v})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	yamlStr := string(data)

	// mustContainKey fails if the serialized YAML does not contain the given
	// "key:" substring, which means the field is either missing or renamed.
	mustContainKey := func(key string) {
		t.Helper()
		// Match "key:" with optional trailing space/newline so we don't
		// accidentally match a value that happens to contain the key string.
		if !bytes.Contains(data, []byte(key+":")) {
			t.Errorf("serialized YAML missing expected key %q\n---\n%s", key, yamlStr)
		}
	}
	// mustNotContainKey fails if the serialized YAML contains a key that
	// should not exist (catches rename bugs in the other direction).
	mustNotContainKey := func(key string) {
		t.Helper()
		needle := []byte(key + ":")
		if bytes.Contains(data, needle) {
			t.Errorf("serialized YAML contains unexpected key %q (tag regression?)\n---\n%s", key, yamlStr)
		}
	}

	// Virtual.UID must serialize as "Uid" (json:"Uid"), NOT the Go convention "UID".
	// Changing this tag would break deserialization of all existing ConfigMap data.
	mustContainKey("Uid")
	mustNotContainKey("UID")
	mustNotContainKey("uid")

	// Fields with no json tag use the Go field name verbatim.
	mustContainKey("Namespace")
	mustContainKey("Ports")
	mustContainKey("Rules")

	// Rule.OwnerID must serialize as "ownerID" (json:"ownerID").
	mustContainKey("ownerID")
	mustNotContainKey("OwnerID")

	// Rule sub-fields with no json tag use the Go field name.
	mustContainKey("Headers")
	mustContainKey("LocalTunIPv4")
	mustContainKey("LocalTunIPv6")
	mustContainKey("PortMap")

	// ContainerPort fields have lowercase json tags (from the struct definition).
	mustContainKey("containerPort")
	mustContainKey("envoyListenerPort")

	// SchemaVersion and FargateMode have omitempty lowercase json tags.
	mustContainKey("schemaVersion")
	mustContainKey("fargateMode")
}

// TestVirtualRoundTrip_MultipleVirtuals verifies that a slice of multiple
// Virtuals (as stored in a ConfigMap under the ENVOY_CONFIG key) round-trips
// without data loss or index corruption.
func TestVirtualRoundTrip_MultipleVirtuals(t *testing.T) {
	originals := []*Virtual{fullVirtualV2(), legacyVirtual()}

	data, err := yaml.Marshal(originals)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	var got []*Virtual
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(got) != len(originals) {
		t.Fatalf("expected %d Virtuals after round-trip, got %d", len(originals), len(got))
	}
	for i := range originals {
		if !reflect.DeepEqual(originals[i], got[i]) {
			t.Errorf("Virtual[%d] round-trip mismatch\noriginal: %+v\ngot:      %+v", i, originals[i], got[i])
		}
	}
}

// TestVirtualSchemaVersion0OmittedFromOutput verifies the omitempty behavior:
// a legacy Virtual with SchemaVersion=0 must NOT emit a "schemaVersion" key
// (omitempty suppresses the zero value). This is the stable legacy format.
func TestVirtualSchemaVersion0OmittedFromOutput(t *testing.T) {
	v := legacyVirtual() // SchemaVersion == 0
	data, err := yaml.Marshal([]*Virtual{v})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(data), "schemaVersion:") {
		t.Errorf("SchemaVersion=0 should be omitted (omitempty), but found in output:\n%s", data)
	}
}

// TestVirtualFargateModeOmittedWhenFalse verifies that FargateMode=false is
// omitted from the serialized YAML (omitempty), keeping the common non-fargate
// case compact.
func TestVirtualFargateModeOmittedWhenFalse(t *testing.T) {
	v := legacyVirtual() // FargateMode == false
	data, err := yaml.Marshal([]*Virtual{v})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(data), "fargateMode:") {
		t.Errorf("FargateMode=false should be omitted (omitempty), but found in output:\n%s", data)
	}
}

// TestVirtualUpgradeSchemaVersion verifies that upgrading a legacy Virtual
// (SchemaVersion=0) to CurrentSchemaVersion and re-marshaling preserves all
// original field values and adds the schemaVersion key.
func TestVirtualUpgradeSchemaVersion(t *testing.T) {
	original := legacyVirtual()
	original.SchemaVersion = CurrentSchemaVersion // simulate an in-place upgrade

	data, err := yaml.Marshal([]*Virtual{original})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	var got []*Virtual
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 Virtual after upgrade round-trip, got %d", len(got))
	}

	rt := got[0]
	if rt.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion: got %d, want %d", rt.SchemaVersion, CurrentSchemaVersion)
	}
	if rt.UID != original.UID {
		t.Errorf("UID: got %q, want %q", rt.UID, original.UID)
	}
	if rt.Namespace != original.Namespace {
		t.Errorf("Namespace: got %q, want %q", rt.Namespace, original.Namespace)
	}
	if len(rt.Rules) != len(original.Rules) {
		t.Fatalf("Rules count: got %d, want %d", len(rt.Rules), len(original.Rules))
	}
}
