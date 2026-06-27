package controlplane

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// legacyYAML represents a ConfigMap value created before SchemaVersion was
// introduced.  The field is absent, so it must deserialize to 0.
const legacyYAML = `- uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "10.0.0.1"
    localtunipv6: "fd00::1"
    portmap:
      8080: "9090"
`

// v1YAML is the same payload but carries an explicit schemaVersion field.
const v1YAML = `- schemaVersion: 1
  uid: deployments.apps.web
  namespace: default
  ports:
  - containerPort: 8080
    protocol: TCP
  rules:
  - headers:
      version: v1
    localtunipv4: "10.0.0.1"
    localtunipv6: "fd00::1"
    portmap:
      8080: "9090"
`

// TestSchemaVersion_LegacyDeserializesToZero verifies that a ConfigMap created
// before the SchemaVersion field was added deserializes with SchemaVersion=0.
func TestSchemaVersion_LegacyDeserializesToZero(t *testing.T) {
	configs, err := parseYaml(legacyYAML)
	if err != nil {
		t.Fatalf("parseYaml(legacyYAML): %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	v := configs[0]
	if v.SchemaVersion != 0 {
		t.Fatalf("legacy config: expected SchemaVersion=0, got %d", v.SchemaVersion)
	}
	assertVirtualFields(t, v)
}

// TestSchemaVersion_V1DeserializesCorrectly verifies that a ConfigMap with
// schemaVersion=1 round-trips through YAML correctly.
func TestSchemaVersion_V1DeserializesCorrectly(t *testing.T) {
	configs, err := parseYaml(v1YAML)
	if err != nil {
		t.Fatalf("parseYaml(v1YAML): %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	v := configs[0]
	if v.SchemaVersion != 1 {
		t.Fatalf("v1 config: expected SchemaVersion=1, got %d", v.SchemaVersion)
	}
	assertVirtualFields(t, v)
}

// TestSchemaVersion_ReserializeLegacyPreservesFields verifies that
// re-marshaling a legacy (SchemaVersion=0) config preserves every original
// field and, once SchemaVersion is set, includes it in the output.
func TestSchemaVersion_ReserializeLegacyPreservesFields(t *testing.T) {
	configs, err := parseYaml(legacyYAML)
	if err != nil {
		t.Fatalf("parseYaml: %v", err)
	}
	v := configs[0]

	// Simulate upgrading the config to the current schema version.
	v.SchemaVersion = CurrentSchemaVersion

	out, err := yaml.Marshal([]*Virtual{v})
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	// Re-parse the serialized output and verify all fields survived.
	roundTripped, err := parseYaml(string(out))
	if err != nil {
		t.Fatalf("re-parse after marshal: %v", err)
	}
	if len(roundTripped) != 1 {
		t.Fatalf("expected 1 config after round-trip, got %d", len(roundTripped))
	}

	rt := roundTripped[0]
	if rt.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("round-trip: expected SchemaVersion=%d, got %d", CurrentSchemaVersion, rt.SchemaVersion)
	}
	assertVirtualFields(t, rt)

	// Verify the raw YAML text contains the schemaVersion key.
	if !containsKey(string(out), "schemaVersion") {
		t.Fatalf("re-serialized YAML missing schemaVersion key:\n%s", out)
	}
}

// assertVirtualFields checks that the core fields from the sample YAML were
// deserialized correctly, independent of schema version.
func assertVirtualFields(t *testing.T, v *Virtual) {
	t.Helper()

	if v.UID != "deployments.apps.web" {
		t.Errorf("UID: got %q, want %q", v.UID, "deployments.apps.web")
	}
	if v.Namespace != "default" {
		t.Errorf("Namespace: got %q, want %q", v.Namespace, "default")
	}
	if len(v.Ports) != 1 {
		t.Fatalf("Ports: got %d entries, want 1", len(v.Ports))
	}
	if v.Ports[0].ContainerPort != 8080 {
		t.Errorf("Ports[0].ContainerPort: got %d, want 8080", v.Ports[0].ContainerPort)
	}
	if v.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("Ports[0].Protocol: got %q, want %q", v.Ports[0].Protocol, corev1.ProtocolTCP)
	}
	if len(v.Rules) != 1 {
		t.Fatalf("Rules: got %d entries, want 1", len(v.Rules))
	}

	rule := v.Rules[0]
	if rule.Headers["version"] != "v1" {
		t.Errorf("Rules[0].Headers[version]: got %q, want %q", rule.Headers["version"], "v1")
	}
	if rule.LocalTunIPv4 != "10.0.0.1" {
		t.Errorf("LocalTunIPv4: got %q, want %q", rule.LocalTunIPv4, "10.0.0.1")
	}
	if rule.LocalTunIPv6 != "fd00::1" {
		t.Errorf("LocalTunIPv6: got %q, want %q", rule.LocalTunIPv6, "fd00::1")
	}
	if len(rule.PortMap) != 1 {
		t.Fatalf("PortMap: got %d entries, want 1", len(rule.PortMap))
	}
	if rule.PortMap[8080] != "9090" {
		t.Errorf("PortMap[8080]: got %q, want %q", rule.PortMap[8080], "9090")
	}
}

// containsKey does a simple substring check for a YAML key in serialized output.
func containsKey(yamlText, key string) bool {
	// Look for key followed by colon, which covers both "key:" and "key: value".
	for i := 0; i <= len(yamlText)-len(key)-1; i++ {
		if yamlText[i:i+len(key)] == key && yamlText[i+len(key)] == ':' {
			return true
		}
	}
	return false
}
