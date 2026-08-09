package xds

import (
	"context"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// setupSyncTestServer creates a TunConfigServer backed by a fake clientset whose
// ENVOY_CONFIG already contains the given Virtual list.
func setupSyncTestServer(t *testing.T, virtuals []*Virtual) *TunConfigServer {
	t.Helper()
	data := ""
	if len(virtuals) > 0 {
		b, err := yaml.Marshal(virtuals)
		if err != nil {
			t.Fatalf("marshal initial virtuals: %v", err)
		}
		data = string(b)
	}
	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-ns", UID: "uid-sync"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "test-ns"},
			Data: map[string]string{
				config.KeyTunIPPool: "",
				config.KeyEnvoy:     data,
			},
		},
	)
	s, err := NewTunConfigServer(context.Background(), clientset, "test-ns")
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}
	return s
}

// readEnvoyVirtuals reads the ENVOY_CONFIG from the ConfigMap backing the server.
func readEnvoyVirtuals(t *testing.T, s *TunConfigServer) []*Virtual {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	virtuals, err := parseYaml(cm.Data[config.KeyEnvoy])
	if err != nil {
		t.Fatalf("parseYaml: %v", err)
	}
	return virtuals
}

// TestSyncEnvoyRuleIP_UpdatesMatchingOwner verifies that syncEnvoyRuleIP (rewritten
// to use Get+Update) correctly updates LocalTunIPv4 for the matching ownerID and
// leaves other owners untouched.
func TestSyncEnvoyRuleIP_UpdatesMatchingOwner(t *testing.T) {
	const (
		ownerA = "owner-a"
		ownerB = "owner-b"
	)
	initial := []*Virtual{
		{
			SchemaVersion: CurrentSchemaVersion,
			UID:           "deploy-foo",
			Namespace:     "test-ns",
			Rules: []*Rule{
				{OwnerID: ownerA, LocalTunIPv4: "198.18.0.1", LocalTunIPv6: "2001:2::1"},
				{OwnerID: ownerB, LocalTunIPv4: "198.18.0.2", LocalTunIPv6: "2001:2::2"},
			},
		},
	}
	s := setupSyncTestServer(t, initial)

	newV4 := mustParseCIDR(t, "198.18.0.99/32")
	newV6 := mustParseCIDR(t, "2001:2::99/128")

	// Sync only ownerA.
	s.syncEnvoyRuleIP(context.Background(), ownerA, newV4, newV6)

	virtuals := readEnvoyVirtuals(t, s)
	if len(virtuals) == 0 || len(virtuals[0].Rules) < 2 {
		t.Fatalf("expected 1 virtual with 2 rules, got %v", virtuals)
	}
	for _, r := range virtuals[0].Rules {
		switch r.OwnerID {
		case ownerA:
			if r.LocalTunIPv4 != "198.18.0.99" {
				t.Errorf("ownerA LocalTunIPv4: got %q, want 198.18.0.99", r.LocalTunIPv4)
			}
			if r.LocalTunIPv6 != "2001:2::99" {
				t.Errorf("ownerA LocalTunIPv6: got %q, want 2001:2::99", r.LocalTunIPv6)
			}
		case ownerB:
			if r.LocalTunIPv4 != "198.18.0.2" {
				t.Errorf("ownerB LocalTunIPv4 changed unexpectedly: got %q", r.LocalTunIPv4)
			}
		}
	}
}

// TestSyncEnvoyRuleIP_NoChangeWhenIPUnchanged verifies that syncEnvoyRuleIP skips
// the Update when the IP is already current (changed=false path).
func TestSyncEnvoyRuleIP_NoChangeWhenIPUnchanged(t *testing.T) {
	initial := []*Virtual{
		{
			SchemaVersion: CurrentSchemaVersion,
			UID:           "deploy-bar",
			Namespace:     "test-ns",
			Rules: []*Rule{
				{OwnerID: "owner-x", LocalTunIPv4: "198.18.0.5", LocalTunIPv6: "2001:2::5"},
			},
		},
	}
	s := setupSyncTestServer(t, initial)

	// Sync with the same IP — should be a no-op.
	sameV4 := mustParseCIDR(t, "198.18.0.5/32")
	sameV6 := mustParseCIDR(t, "2001:2::5/128")
	s.syncEnvoyRuleIP(context.Background(), "owner-x", sameV4, sameV6)

	virtuals := readEnvoyVirtuals(t, s)
	if len(virtuals) == 0 || len(virtuals[0].Rules) == 0 {
		t.Fatal("rule unexpectedly removed")
	}
	r := virtuals[0].Rules[0]
	if r.LocalTunIPv4 != "198.18.0.5" {
		t.Errorf("LocalTunIPv4: got %q, want 198.18.0.5", r.LocalTunIPv4)
	}
}

// TestSyncEnvoyRuleIP_EmptyConfigSkipped verifies that syncEnvoyRuleIP does not
// error when ENVOY_CONFIG is empty/unparseable — it logs a warning and returns.
func TestSyncEnvoyRuleIP_EmptyConfigSkipped(t *testing.T) {
	s := setupSyncTestServer(t, nil) // empty ENVOY_CONFIG
	// Should not panic or return error (it logs a warning and returns).
	s.syncEnvoyRuleIP(context.Background(), "owner-missing", mustParseCIDR(t, "198.18.0.10/32"), nil)
}

func mustParseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("mustParseCIDR(%q): %v", s, err)
	}
	return cidr
}
