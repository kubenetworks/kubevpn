//go:build tun

// End-to-end guard on Linux for the same invariant fix(tun) 858a1503 enforces on macOS: after
// a TUN IP change the OLD address must be gone and the NEW one present. Linux changeIP uses
// netlink (del old + add new); this creates a real TUN device and checks the outcome via
// net.Interface.Addrs.
//
// The _linux_test.go filename constrains this to Linux, so only the `tun` tag is needed
// (linux is implied by GOOS). Requires root (CAP_NET_ADMIN). Run:
//
//	sudo go test ./pkg/tun/ -tags tun -run TestTUN_ChangeIP -v
package tun

import (
	"net"
	"net/netip"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	netlink1 "github.com/vishvananda/netlink"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// linuxIfaceHasAddr reports whether ifName currently has ip assigned. The darwin build has a
// production equivalent (interfaceHasAddr); linux changeIP does not need one, so the test
// carries its own.
func linuxIfaceHasAddr(ifName string, ip netip.Addr) bool {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return false
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			if na, ok := netip.AddrFromSlice(ipNet.IP); ok && na.Unmap() == ip.Unmap() {
				return true
			}
		}
	}
	return false
}

func linuxFindTunName(t *testing.T, ip netip.Addr) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifaces {
		if linuxIfaceHasAddr(ifi.Name, ip) {
			return ifi.Name
		}
	}
	t.Fatalf("no interface carries %s (device not created?)", ip)
	return ""
}

func TestTUN_ChangeIP_RemovesOldAddr(t *testing.T) {
	const (
		v4old = "198.18.0.141/32"
		v4new = "198.18.0.142/32"
		v6old = "2001:2::141/128"
		v6new = "2001:2::142/128"
	)
	ln, err := Listener(Config{Addr: v4old, Addr6: v6old, MTU: config.DefaultMTU})
	if err != nil {
		t.Fatalf("create tun: %v", err)
	}
	defer ln.Close()

	name := linuxFindTunName(t, netip.MustParsePrefix(v4old).Addr())
	t.Logf("tun device: %s", name)

	// IPv4: change and verify old gone / new present.
	if err := ChangeIP(name, v4old, v4new); err != nil {
		t.Fatalf("ChangeIP %s -> %s: %v", v4old, v4new, err)
	}
	if linuxIfaceHasAddr(name, netip.MustParsePrefix(v4old).Addr()) {
		t.Errorf("old IPv4 %s still present on %s after ChangeIP", v4old, name)
	}
	if !linuxIfaceHasAddr(name, netip.MustParsePrefix(v4new).Addr()) {
		t.Errorf("new IPv4 %s missing on %s after ChangeIP", v4new, name)
	}

	// IPv6: same invariant.
	if !linuxIfaceHasAddr(name, netip.MustParsePrefix(v6old).Addr()) {
		t.Skipf("IPv6 %s not assigned (IPv6 may be disabled); skipping IPv6 leg", v6old)
	}
	if err := ChangeIP(name, v6old, v6new); err != nil {
		t.Fatalf("ChangeIP %s -> %s: %v", v6old, v6new, err)
	}
	if linuxIfaceHasAddr(name, netip.MustParsePrefix(v6old).Addr()) {
		t.Errorf("old IPv6 %s still present on %s after ChangeIP", v6old, name)
	}
	if !linuxIfaceHasAddr(name, netip.MustParsePrefix(v6new).Addr()) {
		t.Errorf("new IPv6 %s missing on %s after ChangeIP", v6new, name)
	}
}

// linuxRouteDsts returns the destination prefixes currently attached to ifName, read straight
// from netlink — the Linux counterpart of reading the RIB on macOS.
func linuxRouteDsts(t *testing.T, ifName string) map[string]bool {
	t.Helper()
	link, err := netlink1.LinkByName(ifName)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", ifName, err)
	}
	rl, err := netlink1.RouteList(link, netlink1.FAMILY_ALL)
	if err != nil {
		t.Fatalf("RouteList(%s): %v", ifName, err)
	}
	got := map[string]bool{}
	for _, r := range rl {
		if r.Dst != nil {
			got[r.Dst.String()] = true
		}
	}
	return got
}

// linuxRouteSrc asks the kernel which interface and source address a packet to dst would get —
// the question `ip route get` answers, and the Linux counterpart of reading RTAX_IFA on macOS.
// netlink rather than netroute on purpose: for a route carrying no prefsrc, netroute substitutes
// the first address it finds on the interface, which for IPv6 is usually the link-local one, so
// it would answer a different question than the one under test.
func linuxRouteSrc(t *testing.T, dst string) (string, net.IP) {
	t.Helper()
	rs, err := netlink1.RouteGet(net.ParseIP(dst))
	if err != nil {
		t.Fatalf("RouteGet(%s): %v", dst, err)
	}
	if len(rs) == 0 {
		t.Fatalf("RouteGet(%s): kernel returned no route", dst)
	}
	link, err := netlink1.LinkByIndex(rs[0].LinkIndex)
	if err != nil {
		t.Fatalf("LinkByIndex(%d): %v", rs[0].LinkIndex, err)
	}
	return link.Attrs().Name, rs[0].Src
}

// TestTUN_ChangeIP_RoutesSurviveAndFollowNewAddress answers, on a real device and for both
// address families, whether Linux shares the macOS failure in
// docs/52-stale-route-src-after-ip-change.md.
//
// macOS resolves a route's source address once, when the entry is created, so an IP change
// leaves every earlier route sourcing from an address that is gone. Linux installs these routes
// with no prefsrc (route_linux.go: netlink.AddRoute with an empty source) and picks the source
// at transmit time, so the same change should be invisible here. Two things have to hold for
// that to be true, and neither is obvious from reading the code: the routes must survive the
// deletion of the address they were installed under, and the source the kernel would now choose
// must be the new address.
//
// Both were false for IPv4 before changeIP was reordered to add-before-delete: losing its last
// IPv4 address made the kernel flush the device's whole IPv4 route table, and cluster traffic
// fell back to the default route and left over the physical NIC. The IPv6 leg is here because
// that outcome was guessed wrong once already — a device keeps its fe80::/64 link-local address
// throughout, so it never loses its last IPv6 address, which predicts no flush. Predictions
// about this kernel have not earned any trust; the test is the answer.
func TestTUN_ChangeIP_RoutesSurviveAndFollowNewAddress(t *testing.T) {
	const (
		v4old = "198.18.0.151/32"
		v4new = "198.18.0.152/32"
		v6old = "2001:2::151/128"
		v6new = "2001:2::152/128"
		cidr4 = "10.151.0.0/16"
		host4 = "10.152.0.10/32"
		cidr6 = "2001:db8:151::/64"
	)

	ln, err := Listener(Config{Addr: v4old, Addr6: v6old, MTU: config.DefaultMTU})
	if err != nil {
		t.Fatalf("create tun: %v", err)
	}
	defer ln.Close()

	name := linuxFindTunName(t, netip.MustParsePrefix(v4old).Addr())
	hasV6 := linuxIfaceHasAddr(name, netip.MustParsePrefix(v6old).Addr())
	if !hasV6 {
		t.Logf("IPv6 %s not assigned (IPv6 may be disabled); checking the IPv4 leg only", v6old)
	}

	// Each probe is a destination plus the source address the kernel must pick for it once the
	// device has been renumbered.
	prefixes := []string{cidr4, host4}
	probes := []struct{ dst, wantSrc string }{
		{"10.151.0.1", v4new},
		{"10.152.0.10", v4new},
	}
	if hasV6 {
		prefixes = append(prefixes, cidr6)
		probes = append(probes, struct{ dst, wantSrc string }{"2001:db8:151::1", v6new})
	}

	for _, p := range prefixes {
		_, dst, perr := net.ParseCIDR(p)
		if perr != nil {
			t.Fatalf("ParseCIDR(%s): %v", p, perr)
		}
		if err = AddRoutes(name, types.Route{Dst: *dst}); err != nil {
			t.Fatalf("AddRoutes(%s): %v", p, err)
		}
	}
	installed := linuxRouteDsts(t, name)
	for _, p := range prefixes {
		if !installed[p] {
			t.Fatalf("precondition: route %s not installed on %s, got %v", p, name, installed)
		}
	}

	if err = ChangeIP(name, v4old, v4new); err != nil {
		t.Fatalf("ChangeIP %s -> %s: %v", v4old, v4new, err)
	}
	if hasV6 {
		if err = ChangeIP(name, v6old, v6new); err != nil {
			t.Fatalf("ChangeIP %s -> %s: %v", v6old, v6new, err)
		}
	}

	// A: renumbering must not take the routes with it. If it does, Linux loses its cluster
	// routes on an IP change and nothing puts them back.
	got := linuxRouteDsts(t, name)
	for _, p := range prefixes {
		if !got[p] {
			t.Errorf("route %s vanished when the address changed; Linux needs the darwin-only "+
				"route refresh in NetworkManager.ChangeTunIP (got %v)", p, got)
		}
	}

	// B: the source the kernel would now use must be the new address — the same invariant
	// TestTUN_ChangeIP_RouteSrcFollowsNewAddress checks on macOS, and the value
	// `kubevpn route search` prints as RT_IFA. A wrong interface here means the traffic is
	// leaving the tunnel entirely.
	for _, probe := range probes {
		ifName, routeSrc := linuxRouteSrc(t, probe.dst)
		if ifName != name {
			t.Errorf("packets to %s would leave via %s, not %s — cluster traffic is escaping the tunnel",
				probe.dst, ifName, name)
			continue
		}
		want := net.IP(netip.MustParsePrefix(probe.wantSrc).Addr().AsSlice())
		if !routeSrc.Equal(want) {
			t.Errorf("packets to %s would source from %s, want %s — Linux pins the source like macOS does",
				probe.dst, routeSrc, want)
		}
		// Cross-check against what the kernel picks when a socket actually connects. On Linux
		// the two agree for both families; on macOS they diverge for IPv6, which is why the
		// darwin test asserts on this one. Keeping both here is what makes that claim checkable
		// rather than asserted.
		if dialSrc := kernelSourceFor(t, probe.dst); !dialSrc.Equal(want) {
			t.Errorf("a socket to %s would source from %s, want %s", probe.dst, dialSrc, want)
		}
	}
}
