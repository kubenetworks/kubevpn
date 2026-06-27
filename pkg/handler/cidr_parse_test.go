package handler

import (
	"net"
	"strings"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// parseCachedCIDRs replicates the CIDR cache parsing logic from getCIDR.
// It splits a space-separated string of CIDRs, parses each, accumulates them,
// and applies dedup + API server IP filtering after each addition (matching
// the incremental pattern in getCIDR).
func parseCachedCIDRs(ipPoolStr string, apiServerIPs []net.IP) []*net.IPNet {
	var cidrs []*net.IPNet
	for _, s := range strings.Split(ipPoolStr, " ") {
		_, cidr, _ := net.ParseCIDR(s)
		if cidr != nil {
			cidrs = util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(append(cidrs, cidr)), apiServerIPs)
		}
	}
	return cidrs
}

func TestParseCachedCIDRs(t *testing.T) {
	t.Run("single IPv4 CIDR", func(t *testing.T) {
		result := parseCachedCIDRs("10.96.0.0/12", nil)
		if len(result) != 1 {
			t.Fatalf("expected 1, got %d: %v", len(result), result)
		}
		if result[0].String() != "10.96.0.0/12" {
			t.Fatalf("expected 10.96.0.0/12, got %s", result[0])
		}
	})

	t.Run("multiple space-separated IPv4 CIDRs", func(t *testing.T) {
		result := parseCachedCIDRs("10.233.0.0/18 10.233.64.0/18 172.16.0.0/12", nil)
		if len(result) != 3 {
			t.Fatalf("expected 3, got %d: %v", len(result), result)
		}
	})

	t.Run("empty string", func(t *testing.T) {
		result := parseCachedCIDRs("", nil)
		if len(result) != 0 {
			t.Fatalf("expected 0, got %d: %v", len(result), result)
		}
	})

	t.Run("whitespace only", func(t *testing.T) {
		result := parseCachedCIDRs("   ", nil)
		if len(result) != 0 {
			t.Fatalf("expected 0, got %d: %v", len(result), result)
		}
	})

	t.Run("invalid entries skipped", func(t *testing.T) {
		result := parseCachedCIDRs("10.96.0.0/12 not-a-cidr 172.16.0.0/12", nil)
		if len(result) != 2 {
			t.Fatalf("expected 2, got %d: %v", len(result), result)
		}
	})

	t.Run("overlapping CIDRs deduplicated", func(t *testing.T) {
		result := parseCachedCIDRs("10.0.0.0/16 10.0.1.0/24", nil)
		if len(result) != 1 {
			t.Fatalf("expected 1 after dedup, got %d: %v", len(result), result)
		}
		if result[0].String() != "10.0.0.0/16" {
			t.Fatalf("expected 10.0.0.0/16, got %s", result[0])
		}
	})

	t.Run("duplicate CIDRs collapsed", func(t *testing.T) {
		result := parseCachedCIDRs("10.96.0.0/12 10.96.0.0/12", nil)
		if len(result) != 1 {
			t.Fatalf("expected 1 after dedup, got %d: %v", len(result), result)
		}
	})

	t.Run("API server IP filters matching CIDR", func(t *testing.T) {
		apiIPs := []net.IP{net.ParseIP("10.0.0.1")}
		result := parseCachedCIDRs("10.0.0.0/24 192.168.0.0/16", apiIPs)
		if len(result) != 1 {
			t.Fatalf("expected 1 after filtering, got %d: %v", len(result), result)
		}
		if result[0].String() != "192.168.0.0/16" {
			t.Fatalf("expected 192.168.0.0/16, got %s", result[0])
		}
	})

	t.Run("IPv6 CIDRs parsed", func(t *testing.T) {
		result := parseCachedCIDRs("fd00::/48 2001:db8::/32", nil)
		if len(result) != 2 {
			t.Fatalf("expected 2, got %d: %v", len(result), result)
		}
	})

	t.Run("IPv6 overlapping deduplicated", func(t *testing.T) {
		result := parseCachedCIDRs("fd00::/48 fd00:0:0:1::/64", nil)
		if len(result) != 1 {
			t.Fatalf("expected 1 after dedup, got %d: %v", len(result), result)
		}
		if result[0].String() != "fd00::/48" {
			t.Fatalf("expected fd00::/48, got %s", result[0])
		}
	})

	t.Run("mixed IPv4 and IPv6", func(t *testing.T) {
		result := parseCachedCIDRs("10.233.0.0/18 fd00::/48 172.16.0.0/12 2001:db8::/32", nil)
		if len(result) != 4 {
			t.Fatalf("expected 4, got %d: %v", len(result), result)
		}
	})

	t.Run("mixed with IPv6 overlap", func(t *testing.T) {
		result := parseCachedCIDRs("10.233.0.0/18 fd00::/48 fd00:0:0:1::/64 172.16.0.0/12", nil)
		// fd00:0:0:1::/64 is inside fd00::/48, should be deduped
		if len(result) != 3 {
			t.Fatalf("expected 3, got %d: %v", len(result), result)
		}
	})

	t.Run("IPv6 API server IP filters matching CIDR", func(t *testing.T) {
		apiIPs := []net.IP{net.ParseIP("fd00::1")}
		result := parseCachedCIDRs("fd00::/64 10.96.0.0/12", apiIPs)
		if len(result) != 1 {
			t.Fatalf("expected 1 after filtering, got %d: %v", len(result), result)
		}
		if result[0].String() != "10.96.0.0/12" {
			t.Fatalf("expected 10.96.0.0/12, got %s", result[0])
		}
	})

	t.Run("mixed API server IPs filter both families", func(t *testing.T) {
		apiIPs := []net.IP{
			net.ParseIP("10.0.0.1"),
			net.ParseIP("fd00::1"),
		}
		result := parseCachedCIDRs("10.0.0.0/24 fd00::/64 172.16.0.0/12 2001:db8::/32", apiIPs)
		if len(result) != 2 {
			t.Fatalf("expected 2 after filtering both families, got %d: %v", len(result), result)
		}
	})

	t.Run("trailing space ignored", func(t *testing.T) {
		result := parseCachedCIDRs("10.96.0.0/12 ", nil)
		if len(result) != 1 {
			t.Fatalf("expected 1, got %d: %v", len(result), result)
		}
	})

	t.Run("leading space ignored", func(t *testing.T) {
		result := parseCachedCIDRs(" 10.96.0.0/12", nil)
		if len(result) != 1 {
			t.Fatalf("expected 1, got %d: %v", len(result), result)
		}
	})

	t.Run("multiple spaces between CIDRs", func(t *testing.T) {
		// strings.Split with " " creates empty strings for consecutive spaces;
		// net.ParseCIDR("") returns nil, so they are skipped
		result := parseCachedCIDRs("10.96.0.0/12  172.16.0.0/12", nil)
		if len(result) != 2 {
			t.Fatalf("expected 2, got %d: %v", len(result), result)
		}
	})
}
