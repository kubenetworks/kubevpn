package inject

import (
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
)

func TestAddVirtualRule_Case1_NoExistingEntry(t *testing.T) {
	// Case 1: No existing entry for the given UID/namespace → creates new Virtual
	v := []*controlplane.Virtual{}
	headers := map[string]string{"foo": "bar"}
	portmap := map[int32]string{8080: "9090"}
	ports := []controlplane.ContainerPort{{ContainerPort: 8080, Protocol: "TCP"}}

	result := addVirtualRule(v, "default", "deployments.apps.nginx", ports, headers, "10.0.0.1", "fd00::1", portmap, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	vr := result[0]
	if vr.UID != "deployments.apps.nginx" {
		t.Errorf("expected UID 'deployments.apps.nginx', got %q", vr.UID)
	}
	if vr.Namespace != "default" {
		t.Errorf("expected Namespace 'default', got %q", vr.Namespace)
	}
	if vr.FargateMode != false {
		t.Errorf("expected FargateMode false, got true")
	}
	if len(vr.Ports) != 1 || vr.Ports[0].ContainerPort != 8080 {
		t.Errorf("expected ports [8080], got %+v", vr.Ports)
	}
	if len(vr.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(vr.Rules))
	}
	rule := vr.Rules[0]
	if rule.LocalTunIPv4 != "10.0.0.1" {
		t.Errorf("expected LocalTunIPv4 '10.0.0.1', got %q", rule.LocalTunIPv4)
	}
	if rule.LocalTunIPv6 != "fd00::1" {
		t.Errorf("expected LocalTunIPv6 'fd00::1', got %q", rule.LocalTunIPv6)
	}
	if rule.Headers["foo"] != "bar" {
		t.Errorf("expected header foo=bar, got %v", rule.Headers)
	}
	if rule.PortMap[8080] != "9090" {
		t.Errorf("expected portmap 8080->9090, got %v", rule.PortMap)
	}
}

func TestAddVirtualRule_Case1_PreservesExisting(t *testing.T) {
	// Case 1 with pre-existing entries for other UIDs — new one is appended
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.other",
			Namespace: "default",
			Ports:     []controlplane.ContainerPort{{ContainerPort: 80}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"x": "y"},
				LocalTunIPv4: "10.0.0.99",
				LocalTunIPv6: "fd00::99",
				PortMap:      map[int32]string{80: "8080"},
			}},
		},
	}
	headers := map[string]string{"env": "dev"}
	portmap := map[int32]string{8080: "9090"}
	ports := []controlplane.ContainerPort{{ContainerPort: 8080}}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", ports, headers, "10.0.0.2", "fd00::2", portmap, false)

	if len(result) != 2 {
		t.Fatalf("expected 2 virtuals, got %d", len(result))
	}
	// Original should be untouched
	if result[0].UID != "deployments.apps.other" {
		t.Errorf("expected first entry to remain 'deployments.apps.other', got %q", result[0].UID)
	}
	// New entry appended
	if result[1].UID != "deployments.apps.nginx" {
		t.Errorf("expected second entry 'deployments.apps.nginx', got %q", result[1].UID)
	}
}

func TestAddVirtualRule_Case2_MergeHeadersAndPortmap(t *testing.T) {
	// Case 2: Existing entry with same TUN IP, non-fargate → merges headers and portmap
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.nginx",
			Namespace: "default",
			Ports:     []controlplane.ContainerPort{{ContainerPort: 8080}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"foo": "bar"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090"},
			}},
		},
	}
	// Same TUN IP, add new header and portmap entry
	newHeaders := map[string]string{"env": "dev"}
	newPortmap := map[int32]string{9090: "7070"}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", nil, newHeaders, "10.0.0.1", "fd00::1", newPortmap, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if len(result[0].Rules) != 1 {
		t.Fatalf("expected 1 rule (merged), got %d", len(result[0].Rules))
	}
	rule := result[0].Rules[0]
	// Headers should be merged
	if rule.Headers["foo"] != "bar" {
		t.Errorf("expected merged header foo=bar, got %v", rule.Headers)
	}
	if rule.Headers["env"] != "dev" {
		t.Errorf("expected merged header env=dev, got %v", rule.Headers)
	}
	// PortMap should be merged
	if rule.PortMap[8080] != "9090" {
		t.Errorf("expected portmap 8080->9090 preserved, got %v", rule.PortMap)
	}
	if rule.PortMap[9090] != "7070" {
		t.Errorf("expected portmap 9090->7070 merged, got %v", rule.PortMap)
	}
}

func TestAddVirtualRule_Case2_SkippedInFargateMode(t *testing.T) {
	// Case 2 does NOT apply in fargate mode — even with same TUN IP, it should not merge
	// Instead it falls through to case 3 or 4
	existing := []*controlplane.Virtual{
		{
			UID:         "deployments.apps.nginx",
			Namespace:   "default",
			FargateMode: true,
			Ports:       []controlplane.ContainerPort{{ContainerPort: 8080, EnvoyListenerPort: 15001}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"foo": "bar"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090:7070"},
			}},
		},
	}
	// Same TUN IP and same headers — falls to case 3 (replace)
	newHeaders := map[string]string{"foo": "bar"}
	newPortmap := map[int32]string{8080: "9091:7071"}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", nil, newHeaders, "10.0.0.1", "fd00::1", newPortmap, true)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if len(result[0].Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(result[0].Rules))
	}
	// Case 3 replaces portmap
	rule := result[0].Rules[0]
	if rule.PortMap[8080] != "9091:7071" {
		t.Errorf("expected portmap replaced to 9091:7071, got %v", rule.PortMap)
	}
}

func TestAddVirtualRule_Case3_ReplaceByHeaders(t *testing.T) {
	// Case 3: Existing entry with same headers → replaces TUN IP and portmap
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.nginx",
			Namespace: "default",
			Ports:     []controlplane.ContainerPort{{ContainerPort: 8080}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"foo": "bar"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090"},
			}},
		},
	}
	// Different TUN IP, same headers → replace
	newPortmap := map[int32]string{8080: "7070"}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", nil, map[string]string{"foo": "bar"}, "10.0.0.2", "fd00::2", newPortmap, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if len(result[0].Rules) != 1 {
		t.Fatalf("expected 1 rule (replaced), got %d", len(result[0].Rules))
	}
	rule := result[0].Rules[0]
	if rule.LocalTunIPv4 != "10.0.0.2" {
		t.Errorf("expected replaced LocalTunIPv4 '10.0.0.2', got %q", rule.LocalTunIPv4)
	}
	if rule.LocalTunIPv6 != "fd00::2" {
		t.Errorf("expected replaced LocalTunIPv6 'fd00::2', got %q", rule.LocalTunIPv6)
	}
	if rule.PortMap[8080] != "7070" {
		t.Errorf("expected replaced portmap 8080->7070, got %v", rule.PortMap)
	}
	// Headers remain same
	if rule.Headers["foo"] != "bar" {
		t.Errorf("expected headers unchanged, got %v", rule.Headers)
	}
}

func TestAddVirtualRule_Case4_AppendNewRule(t *testing.T) {
	// Case 4: Different headers, different TUN IP → appends new rule
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.nginx",
			Namespace: "default",
			Ports:     []controlplane.ContainerPort{{ContainerPort: 8080}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"foo": "bar"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090"},
			}},
		},
	}
	// Different headers AND different TUN IP → append
	newHeaders := map[string]string{"env": "staging"}
	newPortmap := map[int32]string{8080: "7070"}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", nil, newHeaders, "10.0.0.2", "fd00::2", newPortmap, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if len(result[0].Rules) != 2 {
		t.Fatalf("expected 2 rules (appended), got %d", len(result[0].Rules))
	}
	// Due to sorting: rules with headers come first, both have headers so order is stable
	// The original rule should still exist
	foundOriginal := false
	foundNew := false
	for _, rule := range result[0].Rules {
		if rule.LocalTunIPv4 == "10.0.0.1" && rule.Headers["foo"] == "bar" {
			foundOriginal = true
		}
		if rule.LocalTunIPv4 == "10.0.0.2" && rule.Headers["env"] == "staging" {
			foundNew = true
			if rule.PortMap[8080] != "7070" {
				t.Errorf("expected new rule portmap 8080->7070, got %v", rule.PortMap)
			}
		}
	}
	if !foundOriginal {
		t.Error("original rule not found after append")
	}
	if !foundNew {
		t.Error("new rule not found after append")
	}
}

func TestAddVirtualRule_Case4_SortingEmptyHeadersLast(t *testing.T) {
	// Verify that after appending, rules with empty headers are sorted last
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.nginx",
			Namespace: "default",
			Ports:     []controlplane.ContainerPort{{ContainerPort: 8080}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{}, // empty headers (default route)
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090"},
			}},
		},
	}
	// Add a rule with non-empty headers
	newHeaders := map[string]string{"version": "v2"}
	newPortmap := map[int32]string{8080: "7070"}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", nil, newHeaders, "10.0.0.2", "fd00::2", newPortmap, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if len(result[0].Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(result[0].Rules))
	}
	// Rule with headers should come first, empty headers last
	if len(result[0].Rules[0].Headers) == 0 {
		t.Error("expected rule with headers to be first (sorted), but got empty headers first")
	}
	if result[0].Rules[0].Headers["version"] != "v2" {
		t.Errorf("expected first rule to have header version=v2, got %v", result[0].Rules[0].Headers)
	}
	if len(result[0].Rules[1].Headers) != 0 {
		t.Errorf("expected last rule to have empty headers, got %v", result[0].Rules[1].Headers)
	}
}

func TestAddVirtualRule_Case1_DifferentNamespace(t *testing.T) {
	// Same UID but different namespace → treated as not found (case 1)
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.nginx",
			Namespace: "production",
			Ports:     []controlplane.ContainerPort{{ContainerPort: 8080}},
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"foo": "bar"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090"},
			}},
		},
	}
	headers := map[string]string{"env": "dev"}
	portmap := map[int32]string{8080: "7070"}
	ports := []controlplane.ContainerPort{{ContainerPort: 8080}}

	result := addVirtualRule(existing, "staging", "deployments.apps.nginx", ports, headers, "10.0.0.2", "fd00::2", portmap, false)

	if len(result) != 2 {
		t.Fatalf("expected 2 virtuals (different namespaces), got %d", len(result))
	}
	if result[0].Namespace != "production" {
		t.Errorf("expected first entry namespace 'production', got %q", result[0].Namespace)
	}
	if result[1].Namespace != "staging" {
		t.Errorf("expected second entry namespace 'staging', got %q", result[1].Namespace)
	}
}

func TestAddVirtualRule_Case4_PortsFilledWhenNil(t *testing.T) {
	// Case 4: When appending a rule, if existing Ports is nil, it gets filled
	existing := []*controlplane.Virtual{
		{
			UID:       "deployments.apps.nginx",
			Namespace: "default",
			Ports:     nil, // nil ports
			Rules: []*controlplane.Rule{{
				Headers:      map[string]string{"foo": "bar"},
				LocalTunIPv4: "10.0.0.1",
				LocalTunIPv6: "fd00::1",
				PortMap:      map[int32]string{8080: "9090"},
			}},
		},
	}
	newHeaders := map[string]string{"env": "dev"}
	newPortmap := map[int32]string{8080: "7070"}
	ports := []controlplane.ContainerPort{{ContainerPort: 8080, Protocol: "TCP"}}

	result := addVirtualRule(existing, "default", "deployments.apps.nginx", ports, newHeaders, "10.0.0.2", "fd00::2", newPortmap, false)

	if len(result) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(result))
	}
	if result[0].Ports == nil {
		t.Error("expected Ports to be filled when nil, but got nil")
	}
	if len(result[0].Ports) != 1 || result[0].Ports[0].ContainerPort != 8080 {
		t.Errorf("expected Ports [8080], got %+v", result[0].Ports)
	}
}
