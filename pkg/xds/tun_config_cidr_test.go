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
}

func TestWarmClusterCIDRCache_SkipsWhenPresent(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: "192.168.0.0/16"}))
	called := false
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet {
		called = true
		return []*net.IPNet{mustCIDR("10.96.0.0/12")}
	})
	if called {
		t.Fatal("detector must NOT run when cache is already populated (never overwrite)")
	}
	if got := readCIDRCache(t, s); got != "192.168.0.0/16" {
		t.Fatalf("cache must be untouched, got %q", got)
	}
}

func TestWarmClusterCIDRCache_NoDetectionLeavesEmpty(t *testing.T) {
	s := newCIDRTestServer(managerCM("default", map[string]string{config.KeyClusterCIDRs: ""}))
	s.warmClusterCIDRCache(context.Background(), func() []*net.IPNet { return nil })
	if got := readCIDRCache(t, s); got != "" {
		t.Fatalf("cache must stay empty when nothing detected (client falls back), got %q", got)
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
