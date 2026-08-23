package xds

import (
	"context"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func newCIDRTestServer(cm *v1.ConfigMap) *TunConfigServer {
	return &TunConfigServer{clientset: fake.NewSimpleClientset(cm), namespace: cm.Namespace}
}

func managerCM(ns string, data map[string]string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
		Data:       data,
	}
}

func readCIDRCache(t *testing.T, s *TunConfigServer) string {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data[config.KeyClusterCIDRs]
}

func readSchemaCache(t *testing.T, s *TunConfigServer) string {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data[config.KeyClusterCIDRsSchema]
}

// Phase 1: an EMPTY cache is filled with the detected CIDRs and stamped with the current
// schema. The detector runs.
func TestWarmClusterCIDRCache_WritesWhenEmpty(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: ""}))
	called := false
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet {
		called = true
		return []*net.IPNet{mustCIDR("10.96.0.0/12"), mustCIDR("10.244.0.0/16")}
	})
	if !called {
		t.Fatal("detector should run when cache is empty")
	}
	got := readCIDRCache(t, s)
	if got == "" {
		t.Fatal("expected cache to be populated")
	}
	// Both detected CIDRs present (order is set-unstable).
	for _, want := range []string{"10.96.0.0/12", "10.244.0.0/16"} {
		if !containsToken(got, want) {
			t.Fatalf("cache %q missing %q", got, want)
		}
	}
	if schema := readSchemaCache(t, s); schema != config.CurrentClusterCIDRsSchema {
		t.Fatalf("schema = %q, want %q (must stamp on fill)", schema, config.CurrentClusterCIDRsSchema)
	}
}

// Phase 3: a populated cache with the CURRENT schema is authoritative — the detector does
// NOT run and the value is left untouched (protects a manual edit / operator-set value).
func TestWarmClusterCIDRCache_SkipsWhenCurrentSchema(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{
		config.KeyClusterCIDRs:       "192.168.0.0/16",
		config.KeyClusterCIDRsSchema: config.CurrentClusterCIDRsSchema,
	}))
	called := false
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet {
		called = true
		return []*net.IPNet{mustCIDR("10.96.0.0/12")}
	})
	if called {
		t.Fatal("detector must NOT run when cache is populated with current schema (never overwrite)")
	}
	if got := readCIDRCache(t, s); got != "192.168.0.0/16" {
		t.Fatalf("cache must be untouched, got %q", got)
	}
}

// Phase 2 — auto-recovery: a populated cache with NO schema (written by a pre-versioning or
// buggy manager/client, e.g. v2.11.6's under-detected GKE Service CIDR) is treated as stale.
// The detector re-runs, and a non-empty result OVERWRITES the stale value and stamps the
// current schema. This is the "cached permanently" half of issue #796.
func TestWarmClusterCIDRCache_RecoversStaleCache(t *testing.T) {
	stale := "34.118.224.0/23" // what v2.11.6 under-detected on GKE
	s := newCIDRTestServer(managerCM("default", map[string]string{
		config.KeyClusterCIDRs: stale,
		// no CLUSTER_CIDRS_SCHEMA -> legacy, older than current
	}))
	called := false
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet {
		called = true
		return []*net.IPNet{mustCIDR("34.118.224.0/20")} // authoritative via dry-run
	})
	if !called {
		t.Fatal("detector must re-run for a stale (no-schema) cache")
	}
	got := readCIDRCache(t, s)
	if !containsToken(got, "34.118.224.0/20") {
		t.Fatalf("stale cache must be overwritten with the authoritative range, got %q", got)
	}
	if containsToken(got, stale) {
		t.Fatalf("stale narrow %q must be replaced, got %q", stale, got)
	}
	if schema := readSchemaCache(t, s); schema != config.CurrentClusterCIDRsSchema {
		t.Fatalf("schema = %q, want %q (must restamp on recovery)", schema, config.CurrentClusterCIDRsSchema)
	}
}

// An empty cache with nothing detected stays empty (client falls back). The detector runs.
func TestWarmClusterCIDRCache_NoDetectionLeavesEmpty(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: ""}))
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet { return nil })
	if got := readCIDRCache(t, s); got != "" {
		t.Fatalf("cache must stay empty when nothing detected (client falls back), got %q", got)
	}
	if schema := readSchemaCache(t, s); schema != "" {
		t.Fatalf("schema must not be stamped when nothing detected, got %q", schema)
	}
}

// Phase 4: a STALE cache (no schema) whose re-detection returns empty is NOT clobbered —
// an empty result never overwrites an existing value. The stale value is left untouched
// rather than wiped to nothing.
func TestWarmClusterCIDRCache_NoDetectionLeavesStaleUntouched(t *testing.T) {
	stale := "34.118.224.0/23"
	s := newCIDRTestServer(managerCM("default", map[string]string{
		config.KeyClusterCIDRs: stale,
	}))
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet { return nil })
	if got := readCIDRCache(t, s); got != stale {
		t.Fatalf("stale cache must be left untouched when re-detection is empty, got %q want %q", got, stale)
	}
	if schema := readSchemaCache(t, s); schema != "" {
		t.Fatalf("schema must not be stamped when re-detection is empty, got %q", schema)
	}
}

func containsToken(s, tok string) bool {
	for _, f := range splitFields(s) {
		if f == tok {
			return true
		}
	}
	return false
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
