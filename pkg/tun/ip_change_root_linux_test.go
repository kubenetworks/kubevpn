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
