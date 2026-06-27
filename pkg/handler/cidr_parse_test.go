package handler

import (
	"net"
	"testing"
)

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
		result := parseCachedCIDRs("10.96.0.0/12  172.16.0.0/12", nil)
		if len(result) != 2 {
			t.Fatalf("expected 2, got %d: %v", len(result), result)
		}
	})
}

func TestEncodeCIDRs(t *testing.T) {
	mustParseCIDR := func(s string) *net.IPNet {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", s, err)
		}
		return cidr
	}

	t.Run("nil input", func(t *testing.T) {
		result := encodeCIDRs(nil)
		if result != "" {
			t.Fatalf("expected empty, got %q", result)
		}
	})

	t.Run("single CIDR", func(t *testing.T) {
		result := encodeCIDRs([]*net.IPNet{mustParseCIDR("10.0.0.0/8")})
		if result != "10.0.0.0/8" {
			t.Fatalf("expected 10.0.0.0/8, got %q", result)
		}
	})

	t.Run("duplicate CIDRs deduplicated", func(t *testing.T) {
		cidrs := []*net.IPNet{mustParseCIDR("10.0.0.0/8"), mustParseCIDR("10.0.0.0/8")}
		result := encodeCIDRs(cidrs)
		if result != "10.0.0.0/8" {
			t.Fatalf("expected single entry, got %q", result)
		}
	})

	t.Run("roundtrip encode-parse", func(t *testing.T) {
		cidrs := []*net.IPNet{mustParseCIDR("10.0.0.0/8"), mustParseCIDR("172.16.0.0/12")}
		encoded := encodeCIDRs(cidrs)
		parsed := parseCachedCIDRs(encoded, nil)
		if len(parsed) != 2 {
			t.Fatalf("roundtrip failed: expected 2, got %d: %v", len(parsed), parsed)
		}
	})
}
