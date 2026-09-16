package netutil

// Tests that a broken, unrelated interface cannot hide our TUN device.
//
// Why this exists: GetTunDevice used to `return nil, err` the moment any interface's Addrs() failed,
// so a single misbehaving NIC made every lookup fail. On macOS awdl0/llw0 (AirDrop/AWDL) come and go
// dynamically and can do exactly that. In production this silently disabled the client's route
// announcement on every reconnect (registrationPayloads) and would equally have made "is my TUN up?"
// report down (daemon/action/status.go). The fix is to skip the unreadable interface and keep looking.

import (
	"errors"
	"net"
	"strings"
	"testing"
)

var errUnreadableNIC = errors.New("simulated awdl0 failure")

// withInterfaceAddrs swaps the addr reader for the duration of a test.
func withInterfaceAddrs(t *testing.T, fn func(net.Interface) ([]net.Addr, error)) {
	t.Helper()
	orig := interfaceAddrs
	interfaceAddrs = fn
	t.Cleanup(func() { interfaceAddrs = orig })
}

// realInterfaces returns the host's interfaces, skipping the test when there are too few to model
// "one NIC is broken, another holds the address we want".
func realInterfaces(t *testing.T) []net.Interface {
	t.Helper()
	ifis, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	if len(ifis) < 2 {
		t.Skipf("need >= 2 interfaces to run this test, host has %d", len(ifis))
	}
	return ifis
}

func TestGetTunDevice_SkipsUnreadableInterface(t *testing.T) {
	ifis := realInterfaces(t)
	broken, target := ifis[0], ifis[len(ifis)-1]
	want := net.ParseIP("198.18.0.5")

	withInterfaceAddrs(t, func(i net.Interface) ([]net.Addr, error) {
		switch i.Index {
		case broken.Index:
			// The interface enumerated BEFORE our target is the one that fails: pre-fix this
			// aborted the scan and the target was never reached.
			return nil, errUnreadableNIC
		case target.Index:
			return []net.Addr{&net.IPNet{IP: want, Mask: net.CIDRMask(32, 32)}}, nil
		default:
			return nil, nil
		}
	})

	got, err := GetTunDevice(want)
	if err != nil {
		t.Fatalf("GetTunDevice returned %v; an unreadable unrelated interface must not hide the device", err)
	}
	if got.Index != target.Index {
		t.Fatalf("found interface %s (index %d), want %s (index %d)", got.Name, got.Index, target.Name, target.Index)
	}
}

func TestGetTunDevice_ReportsSkippedInterfacesWhenNotFound(t *testing.T) {
	ifis := realInterfaces(t)
	broken := ifis[0]

	withInterfaceAddrs(t, func(i net.Interface) ([]net.Addr, error) {
		if i.Index == broken.Index {
			return nil, errUnreadableNIC
		}
		return nil, nil
	})

	_, err := GetTunDevice(net.ParseIP("198.18.0.5"))
	if err == nil {
		t.Fatal("expected a not-found error when no interface holds the address")
	}
	// The skipped interfaces must be nameable from the error alone — that is what makes the next
	// occurrence diagnosable instead of a silent nil.
	if !strings.Contains(err.Error(), broken.Name) {
		t.Fatalf("error %q does not name the skipped interface %q", err, broken.Name)
	}
	if !strings.Contains(err.Error(), errUnreadableNIC.Error()) {
		t.Fatalf("error %q does not carry the underlying cause", err)
	}
}

func TestGetTunDevice_FindsDeviceWithNoBrokenInterfaces(t *testing.T) {
	ifis := realInterfaces(t)
	target := ifis[len(ifis)-1]
	want := net.ParseIP("2001:2::5")

	withInterfaceAddrs(t, func(i net.Interface) ([]net.Addr, error) {
		if i.Index == target.Index {
			return []net.Addr{&net.IPNet{IP: want, Mask: net.CIDRMask(128, 128)}}, nil
		}
		return nil, nil
	})

	got, err := GetTunDevice(net.ParseIP("198.18.0.5"), want)
	if err != nil {
		t.Fatalf("GetTunDevice: %v", err)
	}
	if got.Index != target.Index {
		t.Fatalf("found %s, want %s", got.Name, target.Name)
	}
}
