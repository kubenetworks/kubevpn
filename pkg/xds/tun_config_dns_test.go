package xds

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

const sampleResolv = "nameserver 10.96.0.10\nsearch default.svc.cluster.local svc.cluster.local cluster.local\noptions ndots:5\n"

func readDNSCache(t *testing.T, s *TunConfigServer) string {
	t.Helper()
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data[config.KeyClusterDNS]
}

func TestWarmClusterDNSCache_WritesWhenEmpty(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterDNS: ""}))
	s.warmClusterDNSCache(context.Background(), func() (string, error) { return sampleResolv, nil })
	if got := readDNSCache(t, s); got != sampleResolv {
		t.Fatalf("cache not written: %q", got)
	}
}

func TestWarmClusterDNSCache_SkipsWhenPresent(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterDNS: "nameserver 1.1.1.1\n"}))
	called := false
	s.warmClusterDNSCache(context.Background(), func() (string, error) { called = true; return sampleResolv, nil })
	if called {
		t.Fatal("readResolv must NOT run when the key is already populated (never overwrite)")
	}
	if got := readDNSCache(t, s); got != "nameserver 1.1.1.1\n" {
		t.Fatalf("cache must be untouched, got %q", got)
	}
}

func TestWarmClusterDNSCache_ReadErrorLeavesEmpty(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterDNS: ""}))
	s.warmClusterDNSCache(context.Background(), func() (string, error) { return "", errors.New("boom") })
	if got := readDNSCache(t, s); got != "" {
		t.Fatalf("cache must stay empty on read error (client falls back), got %q", got)
	}
}
