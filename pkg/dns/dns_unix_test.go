//go:build darwin

package dns

import (
	"context"
	"sync"
	"testing"

	miekgdns "github.com/miekg/dns"
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
