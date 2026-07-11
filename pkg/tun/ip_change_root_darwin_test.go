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
