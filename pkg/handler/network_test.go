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

// tunIPConflicts: a pushed IP that matches a sibling/local IP is a conflict, but
// the connection's OWN current TUN IP must never count (else a rollback that
// re-pushes our current IP would loop forever).
func TestTunIPConflicts(t *testing.T) {
	ipn := func(s string) *net.IPNet {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		n.IP = ip
		return n
	}
	ownV4 := ipn("198.18.0.10/16")
	siblingV4 := "198.18.0.20"
	excludes := []string{ownV4.IP.String(), siblingV4, "10.0.0.1"}

	// Pushed IP collides with a sibling → conflict.
	if !tunIPConflicts(ipn("198.18.0.20/16"), nil, ownV4, nil, excludes) {
		t.Error("expected conflict for sibling IP 198.18.0.20")
	}
	// Pushed IP equals our own current IP → NOT a conflict.
	if tunIPConflicts(ipn("198.18.0.10/16"), nil, ownV4, nil, excludes) {
		t.Error("own current IP must not be a conflict")
	}
	// Pushed IP not in excludes → no conflict.
	if tunIPConflicts(ipn("198.18.0.99/16"), nil, ownV4, nil, excludes) {
		t.Error("unrelated IP must not be a conflict")
	}
	// IPv6 conflict path.
	ownV6 := ipn("2001:2::10/64")
	ex6 := []string{ownV6.IP.String(), "2001:2::20"}
	if !tunIPConflicts(nil, ipn("2001:2::20/64"), nil, ownV6, ex6) {
		t.Error("expected conflict for sibling IPv6 2001:2::20")
	}
	if tunIPConflicts(nil, ipn("2001:2::10/64"), nil, ownV6, ex6) {
		t.Error("own current IPv6 must not be a conflict")
	}
}
