//go:build linux

package dns

import (
	"testing"

	miekgdns "github.com/miekg/dns"
)

// TestBuildLibraryOSConfig_IsSplitNotGlobal guards the split-vs-global invariant.
//
// The tailscale library treats an empty MatchDomains as "make this interface the global
// default DNS route" (resolved.go: SetLinkDefaultRoute(_, len(MatchDomains) == 0)). If the
// OSConfig we hand it ever loses its MatchDomains, cluster DNS silently hijacks ALL queries
// on the host instead of only cluster domains. This test fails loudly if that regresses.
func TestBuildLibraryOSConfig_IsSplitNotGlobal(t *testing.T) {
	search := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	cfg := buildLibraryOSConfig(&miekgdns.ClientConfig{
		Servers: []string{"172.28.64.10"},
		Search:  search,
	})

	if len(cfg.MatchDomains) == 0 {
		t.Fatal("MatchDomains is empty: DNS would be configured as a GLOBAL default route, not split")
	}
	if len(cfg.MatchDomains) != len(cfg.SearchDomains) {
		t.Fatalf("MatchDomains (%d) must mirror SearchDomains (%d) so only cluster domains route to the TUN",
			len(cfg.MatchDomains), len(cfg.SearchDomains))
	}
	if len(cfg.MatchDomains) != len(search) {
		t.Fatalf("expected %d match domains, got %d", len(search), len(cfg.MatchDomains))
	}
	for i, want := range search {
		if got := cfg.MatchDomains[i].WithoutTrailingDot(); got != want {
			t.Errorf("MatchDomains[%d] = %q, want %q", i, got, want)
		}
		if got := cfg.SearchDomains[i].WithoutTrailingDot(); got != want {
			t.Errorf("SearchDomains[%d] = %q, want %q", i, got, want)
		}
	}
}

// TestBuildLibraryOSConfig_ParsesNameservers verifies valid nameservers are collected and
// unparseable entries are skipped rather than producing an invalid config.
func TestBuildLibraryOSConfig_ParsesNameservers(t *testing.T) {
	cfg := buildLibraryOSConfig(&miekgdns.ClientConfig{
		Servers: []string{"10.96.0.10", "not-an-ip", "fd00::10"},
		Search:  []string{"cluster.local"},
	})

	if len(cfg.Nameservers) != 2 {
		t.Fatalf("expected 2 valid nameservers (invalid one skipped), got %d: %v", len(cfg.Nameservers), cfg.Nameservers)
	}
	if cfg.Nameservers[0].String() != "10.96.0.10" {
		t.Errorf("Nameservers[0] = %q, want 10.96.0.10", cfg.Nameservers[0].String())
	}
}
