package dhcp

import (
	"context"
	"net"
	"testing"
)

// TestRentSpecificIP_AndInRange covers the strict specific-IP allocation used by
// the manual TUN_ALLOCS reconcile path, plus the InRange range check.
func TestRentSpecificIP_AndInRange(t *testing.T) {
	ctx := context.Background()
	m := newFakeManager(t)
	if err := m.InitDHCP(ctx); err != nil {
		t.Fatalf("InitDHCP: %v", err)
	}

	v4 := net.ParseIP("198.18.5.5")

	// in-range free IP succeeds and becomes allocated
	if err := m.RentSpecificIP(ctx, v4, nil); err != nil {
		t.Fatalf("RentSpecificIP fresh: %v", err)
	}
	found := false
	_ = m.ForEach(ctx, func(ip net.IP) {
		if ip.Equal(v4) {
			found = true
		}
	}, func(net.IP) {})
	if !found {
		t.Fatalf("expected %s allocated in bitmap", v4)
	}

	// renting the same IP again fails (already allocated)
	if err := m.RentSpecificIP(ctx, v4, nil); err == nil {
		t.Fatal("expected error renting an already-allocated IP")
	}

	// out-of-range IP fails
	if err := m.RentSpecificIP(ctx, net.ParseIP("10.0.0.1"), nil); err == nil {
		t.Fatal("expected error renting an out-of-range IP")
	}

	// InRange sanity
	if !m.InRange(net.ParseIP("198.18.0.1")) {
		t.Fatal("198.18.0.1 should be in range")
	}
	if m.InRange(net.ParseIP("10.0.0.1")) {
		t.Fatal("10.0.0.1 should be out of range")
	}
	if m.InRange(nil) {
		t.Fatal("nil should not be in range")
	}
}
