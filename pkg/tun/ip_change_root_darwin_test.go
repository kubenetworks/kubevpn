//go:build tun

// End-to-end guards for fix(tun) 858a1503 on macOS: after a TUN IP hot-change, the OLD
// address must actually be gone from the device — not left aliased, which is what made the
// client keep sourcing outbound traffic from the stale IP. These create a real utun device
// and exercise the real SIOCDIFADDR / SIOCDIFADDR_IN6 delete path plus changeIP's verify.
//
// The _darwin_test.go filename already constrains this to macOS, so only the `tun` tag is
// needed (darwin is implied by GOOS). Requires root. Run:
//
//	sudo go test ./pkg/tun/ -tags tun -run TestTUN_ChangeIP -v
package tun

import (
	"net"
	"net/netip"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// findIfaceByAddr returns the name of the interface currently carrying ip (via the same
// net.Interface.Addrs path changeIP verifies with), failing the test if none does.
func findIfaceByAddr(t *testing.T, ip netip.Addr) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifaces {
		if interfaceHasAddr(ifi.Name, ip) {
			return ifi.Name
		}
	}
	t.Fatalf("no interface carries %s (device not created?)", ip)
	return ""
}

// newTestTun creates a real utun with the given v4/v6 addresses and returns its interface name.
func newTestTun(t *testing.T, v4, v6 string) (name string, close func()) {
	t.Helper()
	ln, err := Listener(Config{Addr: v4, Addr6: v6, MTU: config.DefaultMTU})
	if err != nil {
		t.Fatalf("create tun (v4=%s v6=%s): %v", v4, v6, err)
	}
	// Locate the device by an address it must now carry.
	locate := v4
	if locate == "" {
		locate = v6
	}
	name = findIfaceByAddr(t, netip.MustParsePrefix(locate).Addr())
	return name, func() { _ = ln.Close() }
}

func mustHave(t *testing.T, name string, cidr string) {
	t.Helper()
	if a := netip.MustParsePrefix(cidr).Addr(); !interfaceHasAddr(name, a) {
		t.Errorf("%s: expected %s present, but it is missing", name, a)
	}
}

func mustNotHave(t *testing.T, name string, cidr string) {
	t.Helper()
	if a := netip.MustParsePrefix(cidr).Addr(); interfaceHasAddr(name, a) {
		t.Errorf("%s: stale address %s still present after change", name, a)
	}
}

// TestTUN_ChangeIP_RemovesStaleIPv6 is the core regression: SIOCDIFADDR_IN6 must actually
// remove the old IPv6 alias.
func TestTUN_ChangeIP_RemovesStaleIPv6(t *testing.T) {
	const v4 = "198.18.0.111/32"
	const v6old = "2001:2::111/128"
	const v6new = "2001:2::112/128"

	name, close := newTestTun(t, v4, v6old)
	defer close()

	if err := ChangeIP(name, v6old, v6new); err != nil {
		t.Fatalf("ChangeIP(%s -> %s): %v", v6old, v6new, err)
	}
	mustNotHave(t, name, v6old) // the bug: old alias lingered
	mustHave(t, name, v6new)
	mustHave(t, name, v4) // untouched family stays
}

// TestTUN_ChangeIP_RemovesStaleIPv4 guards the IPv4 SIOCDIFADDR path (the family that
// actually regressed in the field).
func TestTUN_ChangeIP_RemovesStaleIPv4(t *testing.T) {
	const v4old = "198.18.0.121/32"
	const v4new = "198.18.0.122/32"
	const v6 = "2001:2::121/128"

	name, close := newTestTun(t, v4old, v6)
	defer close()

	if err := ChangeIP(name, v4old, v4new); err != nil {
		t.Fatalf("ChangeIP(%s -> %s): %v", v4old, v4new, err)
	}
	mustNotHave(t, name, v4old)
	mustHave(t, name, v4new)
	mustHave(t, name, v6)
}

// TestTUN_ChangeIP_DualStack mirrors NetworkManager.ChangeTunIP (which calls ChangeIP per
// family): both old addresses must be gone, both new present.
func TestTUN_ChangeIP_DualStack(t *testing.T) {
	const v4old = "198.18.0.131/32"
	const v4new = "198.18.0.132/32"
	const v6old = "2001:2::131/128"
	const v6new = "2001:2::132/128"

	name, close := newTestTun(t, v4old, v6old)
	defer close()

	if err := ChangeIP(name, v4old, v4new); err != nil {
		t.Fatalf("ChangeIP v4: %v", err)
	}
	if err := ChangeIP(name, v6old, v6new); err != nil {
		t.Fatalf("ChangeIP v6: %v", err)
	}
	mustNotHave(t, name, v4old)
	mustNotHave(t, name, v6old)
	mustHave(t, name, v4new)
	mustHave(t, name, v6new)
}

// routeSrcFor returns the source address the kernel pinned into the route for prefix on
// ifName (RTAX_IFA) — the value `kubevpn route search` prints as RT_IFA, and the address
// outbound packets matching that route actually carry.
func routeSrcFor(t *testing.T, ifName, prefix string) net.IP {
	t.Helper()
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		t.Fatalf("InterfaceByName(%s): %v", ifName, err)
	}
	rib, err := route.FetchRIB(unix.AF_UNSPEC, route.RIBTypeRoute, 0)
	if err != nil {
		t.Fatalf("FetchRIB: %v", err)
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		t.Fatalf("ParseRIB: %v", err)
	}
	want := netip.MustParsePrefix(prefix)
	for _, msg := range msgs {
		m, ok := msg.(*route.RouteMessage)
		if !ok || m.Index != ifi.Index {
			continue
		}
		dst, ok := ipFromRouteAddr(routeAddrAt(m.Addrs, unix.RTAX_DST))
		if !ok || !dst.Equal(net.IP(want.Addr().AsSlice())) {
			continue
		}
		if ones, _ := maskFromRouteAddr(routeAddrAt(m.Addrs, unix.RTAX_NETMASK), len(dst)).Size(); ones != want.Bits() {
			continue
		}
		ifa, _ := ipFromRouteAddr(routeAddrAt(m.Addrs, unix.RTAX_IFA))
		return ifa
	}
	t.Fatalf("no route for %s on %s", prefix, ifName)
	return nil
}

func mustIPNet(t *testing.T, cidr string) net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR(%s): %v", cidr, err)
	}
	return *n
}

// addrOf returns the bare address of a CIDR string, as net.IP, for comparing against a route's
// pinned source.
func addrOf(cidr string) net.IP {
	return net.IP(netip.MustParsePrefix(cidr).Addr().AsSlice())
}

// TestTUN_ChangeIP_RouteSrcFollowsNewAddress is the regression for
// docs/52-stale-route-src-after-ip-change.md: deleting the old address is not enough, because
// routes installed before the change keep sourcing packets from it. A connection in that state
// looks perfectly healthy — the device holds the new address, every cluster prefix still routes
// through the TUN — and carries nothing in either direction.
//
// It covers the things the earlier fix missed: that host routes are refreshed and not just the
// cluster CIDR, that re-asserting an existing route actually rewrites its source address
// instead of being swallowed as EEXIST, and that both address families come through.
//
// The assertion is on the source address the kernel would actually use (kernelSourceFor), not
// on the route's stored ifa field. The two differ on this platform: BSD hands rt_ifa straight
// to ip_output for IPv4, but for IPv6 it pins the interface's link-local address into the route
// and runs in6_selectsrc at transmit instead. An earlier version of this test asserted on the
// field and failed against a perfectly healthy device.
func TestTUN_ChangeIP_RouteSrcFollowsNewAddress(t *testing.T) {
	const (
		v4old = "198.18.0.141/32"
		v4new = "198.18.0.142/32"
		v6old = "2001:2::141/128"
		v6new = "2001:2::142/128"
	)
	// Each cluster prefix, a destination inside it, and the TUN address packets to that
	// destination must be sourced from before and after the device is renumbered.
	routeCases := []struct{ prefix, probe, oldSrc, newSrc string }{
		{"10.144.0.0/16", "10.144.0.1", v4old, v4new},
		{"10.145.0.10/32", "10.145.0.10", v4old, v4new},
		{"2001:db8:144::/64", "2001:db8:144::1", v6old, v6new},
		{"2001:db8:145::10/128", "2001:db8:145::10", v6old, v6new},
	}

	name, closeTun := newTestTun(t, v4old, v6old)
	defer closeTun()

	for _, rc := range routeCases {
		dst := mustIPNet(t, rc.prefix)
		if err := AddRoutes(name, types.Route{Dst: dst}); err != nil {
			t.Fatalf("AddRoutes(%s): %v", rc.prefix, err)
		}
		t.Cleanup(func() { _ = DeleteRoutes(name, types.Route{Dst: dst}) })
	}

	// listTunRoutes must find all of them, in both families, and only ours: the kernel's own
	// scope entries for the device addresses carry no RTF_STATIC and must not be dragged into
	// a refresh.
	routes, err := listTunRoutes(name)
	if err != nil {
		t.Fatalf("listTunRoutes: %v", err)
	}
	listed := map[string]bool{}
	for _, r := range routes {
		listed[r.Dst.String()] = true
	}
	for _, rc := range routeCases {
		if !listed[rc.prefix] {
			t.Fatalf("listTunRoutes missed %s, got %v", rc.prefix, listed)
		}
	}

	for _, rc := range routeCases {
		if got := kernelSourceFor(t, rc.probe); !got.Equal(addrOf(rc.oldSrc)) {
			t.Fatalf("precondition: packets to %s would source from %s, want %s",
				rc.probe, got, addrOf(rc.oldSrc))
		}
		// Informational: shows which address the kernel pinned into the route, which for IPv6
		// is the link-local one. Recorded, not asserted — see the doc comment.
		t.Logf("route %s rt_ifa=%s (kernel source: %s)",
			rc.prefix, routeSrcFor(t, name, rc.prefix), addrOf(rc.oldSrc))
	}

	if err = ChangeIP(name, v4old, v4new); err != nil {
		t.Fatalf("ChangeIP(%s -> %s): %v", v4old, v4new, err)
	}
	if err = ChangeIP(name, v6old, v6new); err != nil {
		t.Fatalf("ChangeIP(%s -> %s): %v", v6old, v6new, err)
	}
	mustNotHave(t, name, v4old)
	mustHave(t, name, v4new)
	mustNotHave(t, name, v6old)
	mustHave(t, name, v6new)

	// What NetworkManager.refreshTunRoutes does: re-assert every route the device carries.
	if err = AddRoutes(name, routes...); err != nil {
		t.Fatalf("re-assert routes: %v", err)
	}
	for _, rc := range routeCases {
		if got := kernelSourceFor(t, rc.probe); !got.Equal(addrOf(rc.newSrc)) {
			t.Errorf("packets to %s would source from %s after the refresh, want %s",
				rc.probe, got, addrOf(rc.newSrc))
		}
	}

	// IPv4 only: the kernel really does hand rt_ifa to ip_output, so for that family the stored
	// field has to have been rewritten too — that is what the RTM_CHANGE fallback exists for.
	for _, rc := range routeCases {
		if addrOf(rc.newSrc).To4() == nil {
			continue
		}
		if got := routeSrcFor(t, name, rc.prefix); !got.Equal(addrOf(rc.newSrc)) {
			t.Errorf("route %s still has rt_ifa=%s after the refresh, want %s",
				rc.prefix, got, addrOf(rc.newSrc))
		}
	}
}
