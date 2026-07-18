package handler

import (
	"net"
	"slices"
	"testing"
)

// Fix 3: the TUN IP allocator must be told to avoid IPs already held by sibling
// connections (other clusters), in addition to this host's interface IPs.
func TestBuildExcludeIPs_IncludesReservedSiblingIPs(t *testing.T) {
	reserved := func() []net.IP {
		return []net.IP{net.ParseIP("198.18.0.5"), net.ParseIP("2001:2::5")}
	}
	got := buildExcludeIPs(reserved)
	if !slices.Contains(got, "198.18.0.5") {
		t.Errorf("expected reserved IPv4 198.18.0.5 in exclude list, got %v", got)
	}
	if !slices.Contains(got, "2001:2::5") {
		t.Errorf("expected reserved IPv6 2001:2::5 in exclude list, got %v", got)
	}
	// Sanity: it still includes local interface IPs (non-empty on any host).
	if len(got) < 2 {
		t.Errorf("expected local interface IPs plus reserved, got %v", got)
	}
}

// A nil reserved provider must be handled gracefully (single-cluster case).
func TestBuildExcludeIPs_NilReserved(t *testing.T) {
	got := buildExcludeIPs(nil)
	for _, ip := range got {
		if net.ParseIP(ip) == nil {
			t.Errorf("non-IP entry in exclude list: %q", ip)
		}
	}
}
