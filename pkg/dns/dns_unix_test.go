//go:build darwin

package dns

import (
	"context"
	"sync"
	"testing"

	miekgdns "github.com/miekg/dns"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSetupCancelDNS_EmptyServers verifies macOS DNS setup/teardown does not panic when the pod
// resolv.conf yielded no nameserver (Servers is empty) — previously Servers[0] was dereferenced
// unconditionally. Guards the empty-servers fix so macOS matches Linux/Windows.
func TestSetupCancelDNS_EmptyServers(t *testing.T) {
	c := &Config{
		Config: &miekgdns.ClientConfig{Servers: nil, Search: []string{"default.svc.cluster.local"}},
		Lock:   &sync.Mutex{},
	}
	// Neither of these should panic; they should return quickly via the empty-servers guard.
	if err := c.SetupDNS(context.Background()); err != nil {
		t.Fatalf("SetupDNS with empty servers returned error: %v", err)
	}
	c.CancelDNS()
}

func TestGetResolversWithExtendedSearchDomains(t *testing.T) {
	// Test that getResolvers correctly handles the extended search domain list
	// that includes cross-namespace search domains.

	// Simulate what network.ExtendSearchDomains produces
	searchList := []string{
		"kubevpn.svc.cluster.local",
		"svc.cluster.local",
		"default.svc.cluster.local",
		"test.svc.cluster.local",
	}
	nsList := []string{"kubevpn", "default", "test"}
	services := []corev1.Service{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "reviews",
				Namespace: "default",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "details",
				Namespace: "test",
			},
		},
	}

	resolvers := getResolvers(searchList, nsList, services)

	// Verify that all search domains have corresponding resolver files
	wantDomains := []string{
		"kubevpn.svc.cluster.local",
		"svc.cluster.local",
		"default.svc.cluster.local",
		"test.svc.cluster.local",
	}

	for _, want := range wantDomains {
		found := false
		wantFile := "/etc/resolver/" + want
		for _, got := range resolvers {
			if got == wantFile {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected resolver file for domain %q, got %v", want, resolvers)
		}
	}
}

func TestToStringWithSearchDomains(t *testing.T) {
	// Test that toString correctly includes search domains in the output
	config := miekgdns.ClientConfig{
		Servers: []string{"10.96.0.10"},
		Search:  []string{"kubevpn.svc.cluster.local", "default.svc.cluster.local"},
		Port:    "53",
		Ndots:   5,
		Timeout: 5,
	}

	result := toString(config)

	// Verify search line is present
	if !contains(result, "search kubevpn.svc.cluster.local default.svc.cluster.local") {
		t.Errorf("toString output missing search domains: %q", result)
	}

	// Verify nameserver line is present
	if !contains(result, "nameserver 10.96.0.10") {
		t.Errorf("toString output missing nameserver: %q", result)
	}

	// Verify options lines are present
	if !contains(result, "options ndots:5") {
		t.Errorf("toString output missing ndots option: %q", result)
	}
	if !contains(result, "options timeout:5") {
		t.Errorf("toString output missing timeout option: %q", result)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) != -1
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
