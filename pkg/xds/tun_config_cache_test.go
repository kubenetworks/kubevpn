package xds

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// These tests pin the contract of the shared cache-warm-up primitives directly, rather
// than only through the CIDR/DNS warm-up callers. The "additive: never overwrite" rule
// is the safety property that lets multiple manager replicas race to fill a cache key
// without clobbering each other, so it is worth locking at the helper level.
//
// The RetryOnConflict retry path is NOT exercised here: fake.NewSimpleClientset does
// not surface ResourceVersion conflicts (CLAUDE.md fake limitation), so retry.RetryOnConflict
// never retries against a fake. That path is covered by cluster-backed CI.

func TestWriteConfigMapKeyIfAbsent_FillsEmptyKey(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{}))
	if err := s.writeConfigMapKeyIfAbsent(context.Background(), config.KeyClusterCIDRs, "10.96.0.0/12"); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := readCIDRCache(t, s); got != "10.96.0.0/12" {
		t.Fatalf("expected key filled, got %q", got)
	}
}

func TestWriteConfigMapKeyIfAbsent_NeverOverwrites(t *testing.T) {
	existing := "192.168.0.0/16"
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: existing}))
	if err := s.writeConfigMapKeyIfAbsent(context.Background(), config.KeyClusterCIDRs, "10.96.0.0/12"); err != nil {
		t.Fatalf("write returned error on present key: %v", err)
	}
	if got := readCIDRCache(t, s); got != existing {
		t.Fatalf("additive rule violated: present value %q was overwritten with %q", existing, got)
	}
}

func TestWriteConfigMapKeyIfAbsent_WhitespaceOnlyIsAbsent(t *testing.T) {
	// A stray newline must not block a real fill (TrimSpace), matching the cache-warm
	// semantics that treat whitespace-only as empty.
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: "  \n"}))
	if err := s.writeConfigMapKeyIfAbsent(context.Background(), config.KeyClusterCIDRs, "10.96.0.0/12"); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if got := readCIDRCache(t, s); got != "10.96.0.0/12" {
		t.Fatalf("whitespace-only should be treated as absent and filled, got %q", got)
	}
}

func TestConfigMapKeyAbsent(t *testing.T) {
	t.Run("absent when unset", func(t *testing.T) {
		s := newCIDRTestServer(managerCM("default", map[string]string{}))
		absent, err := s.configMapKeyAbsent(context.Background(), config.KeyClusterCIDRs)
		if err != nil || !absent {
			t.Fatalf("expected absent=true nil err, got absent=%v err=%v", absent, err)
		}
	})
	t.Run("present when set", func(t *testing.T) {
		s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: "10.0.0.0/8"}))
		absent, err := s.configMapKeyAbsent(context.Background(), config.KeyClusterCIDRs)
		if err != nil || absent {
			t.Fatalf("expected absent=false nil err, got absent=%v err=%v", absent, err)
		}
	})
	t.Run("absent when whitespace-only", func(t *testing.T) {
		s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: "  "}))
		absent, err := s.configMapKeyAbsent(context.Background(), config.KeyClusterCIDRs)
		if err != nil || !absent {
			t.Fatalf("whitespace-only should be absent, got absent=%v err=%v", absent, err)
		}
	})
}

func TestWriteConfigMapKeyIfAbsent_GetErrorWhenNoConfigMap(t *testing.T) {
	// No manager ConfigMap exists at all: the helper surfaces the Get error rather than
	// silently no-oping, so a missing ConfigMap is not mistaken for an empty cache.
	s := &TunConfigServer{clientset: newCIDRTestServer(managerCM("other", map[string]string{})).clientset, namespace: "default"}
	err := s.writeConfigMapKeyIfAbsent(context.Background(), config.KeyClusterCIDRs, "10.0.0.0/8")
	if err == nil {
		t.Fatal("expected Get error when the manager ConfigMap does not exist")
	}
	// Sanity: the namespace we created the ConfigMap in is not the one the server reads.
	if _, getErr := s.clientset.CoreV1().ConfigMaps("default").Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{}); getErr == nil {
		t.Fatal("precondition: manager ConfigMap should not exist in 'default'")
	}
}
