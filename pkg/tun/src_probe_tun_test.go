//go:build tun

package tun

import (
	"net"
	"testing"
)

// kernelSourceFor returns the source address a packet to dst would actually carry.
//
// It asks by making the kernel do the work: connect(2) on a UDP socket performs the full route
// lookup and source-address selection and sends nothing, so LocalAddr is the kernel's own
// answer. This is deliberately not a read of the route's stored ifa field. On macOS that field
// holds the interface's *link-local* address for IPv6 routes while the packet is sourced from
// the global one (BSD runs in6_selectsrc at transmit for v6, but uses rt_ifa directly for v4),
// so reading it answers a different question than the one that matters — a distinction that
// already sent one round of this investigation down the wrong road.
//
// A route with no usable source address makes the dial fail outright, which is itself the
// failure under test: that is what a renumbered macOS TUN looks like before the routes are
// re-asserted.
func kernelSourceFor(t *testing.T, dst string) net.IP {
	t.Helper()
	ip := net.ParseIP(dst)
	if ip == nil {
		t.Fatalf("invalid probe destination %q", dst)
	}
	network, addr := "udp4", dst+":9"
	if ip.To4() == nil {
		network, addr = "udp6", "["+dst+"]:9"
	}
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("dial %s %s: %v (no usable source address for this route?)", network, dst, err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}
