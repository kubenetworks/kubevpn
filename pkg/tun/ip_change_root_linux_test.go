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
	"github.com/libp2p/go-netroute"
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

// TestTUN_ChangeIP_RoutesSurviveAndFollowNewAddress answers, on a real device, whether Linux
// shares the macOS failure in docs/52-stale-route-src-after-ip-change.md.
//
// macOS resolves a route's source address once, when the entry is created, so an IP change
// leaves every earlier route sourcing from an address that is gone. Linux installs these routes
// with no prefsrc (route_linux.go: netlink.AddRoute with an empty source) and picks the source
// at transmit time, so the same change should be invisible here. Two things have to hold for
// that to be true, and neither is obvious from reading the code: the routes must survive the
// deletion of the address they were installed under, and the source the kernel would now choose
// must be the new address.
//
// This is the test that lets the Linux and Windows halves of docs/52 be stated as fact rather
// than as an argument about what the kernel probably does. Its failure means Linux needs the
// refresh that ChangeTunIP currently performs only on darwin.
func TestTUN_ChangeIP_RoutesSurviveAndFollowNewAddress(t *testing.T) {
	const (
		v4old       = "198.18.0.151/32"
		v4new       = "198.18.0.152/32"
		clusterCIDR = "10.151.0.0/16"
		serviceIP   = "10.152.0.10/32"
	)

	ln, err := Listener(Config{Addr: v4old, MTU: config.DefaultMTU})
	if err != nil {
		t.Fatalf("create tun: %v", err)
	}
	defer ln.Close()

	name := linuxFindTunName(t, netip.MustParsePrefix(v4old).Addr())
	for _, p := range []string{clusterCIDR, serviceIP} {
		_, dst, perr := net.ParseCIDR(p)
		if perr != nil {
			t.Fatalf("ParseCIDR(%s): %v", p, perr)
		}
		if err = AddRoutes(name, types.Route{Dst: *dst}); err != nil {
			t.Fatalf("AddRoutes(%s): %v", p, err)
		}
	}
	if got := linuxRouteDsts(t, name); !got[clusterCIDR] || !got[serviceIP] {
		t.Fatalf("precondition: routes not installed on %s, got %v", name, got)
	}

	if err = ChangeIP(name, v4old, v4new); err != nil {
		t.Fatalf("ChangeIP %s -> %s: %v", v4old, v4new, err)
	}

	// A: deleting the address must not take the routes with it. If it does, Linux loses every
	// cluster route on an IP change and nothing puts them back.
	got := linuxRouteDsts(t, name)
	for _, p := range []string{clusterCIDR, serviceIP} {
		if !got[p] {
			t.Errorf("route %s vanished when the address changed; Linux needs the darwin-only "+
				"route refresh in NetworkManager.ChangeTunIP (got %v)", p, got)
		}
	}

	// B: the source address the kernel would now use must be the new one. This is the same
	// invariant TestTUN_ChangeIP_RouteSrcFollowsNewAddress checks on macOS, and the same value
	// `kubevpn route search` prints as RT_IFA.
	r, err := netroute.New()
	if err != nil {
		t.Fatalf("netroute.New: %v", err)
	}
	wantSrc := net.IP(netip.MustParsePrefix(v4new).Addr().AsSlice())
	for _, probe := range []string{"10.151.0.1", "10.152.0.10"} {
		ifi, _, src, rerr := r.Route(net.ParseIP(probe))
		if rerr != nil {
			t.Errorf("Route(%s): %v", probe, rerr)
			continue
		}
		if ifi.Name != name {
			t.Errorf("Route(%s) resolved to %s, want %s", probe, ifi.Name, name)
			continue
		}
		if !src.Equal(wantSrc) {
			t.Errorf("Route(%s) sources from %s, want %s — Linux pins the source like macOS does",
				probe, src, wantSrc)
		}
	}
}
