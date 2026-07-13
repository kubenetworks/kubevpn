package tun

// This file is darwin-only via its _darwin_test.go filename suffix, so no build tag is needed.

import (
	"net/netip"
	"testing"
)

// TestInterfaceHasAddr covers the pure-Go verification helper changeIP uses to confirm a stale
// TUN address was actually removed (fix(tun) 858a1503). It runs against the loopback interface,
// which always carries 127.0.0.1 and ::1 on macOS, so it needs no root and no TUN device.
func TestInterfaceHasAddr(t *testing.T) {
	cases := []struct {
		name  string
		iface string
		addr  string
		want  bool
	}{
		{"loopback has IPv4", "lo0", "127.0.0.1", true},
		{"loopback has IPv6", "lo0", "::1", true},
		{"loopback lacks arbitrary IPv4", "lo0", "198.18.199.199", false},
		{"loopback lacks arbitrary IPv6", "lo0", "2001:2::dead", false},
		{"unknown interface", "kubevpn-no-such-if0", "::1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interfaceHasAddr(tc.iface, netip.MustParseAddr(tc.addr))
			if got != tc.want {
				t.Errorf("interfaceHasAddr(%q, %s) = %v, want %v", tc.iface, tc.addr, got, tc.want)
			}
		})
	}
}
