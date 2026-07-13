package tun

// This file is darwin-only via its _darwin_test.go filename suffix, so no build tag is needed.

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestInet6DeleteIoctlNumber locks the SIOCDIFADDR_IN6 request number and the in6_ifreq
// layout it is derived from — the load-bearing magic in the IPv6 pure-syscall delete
// (fix(tun) 858a1503). The ioctl number embeds sizeof(struct in6_ifreq); if in6Ifreq's size
// drifts from the macOS kernel's (288 bytes), the recomputed number stops matching and the
// delete silently no-ops (ENOTTY), so this test must fail first and loudly.
func TestInet6DeleteIoctlNumber(t *testing.T) {
	if got := unsafe.Sizeof(in6Ifreq{}); got != sizeofIn6Ifreq {
		t.Fatalf("unsafe.Sizeof(in6Ifreq{}) = %d, want %d (padding must match struct in6_ifreq)", got, sizeofIn6Ifreq)
	}
	if sizeofIn6Ifreq != 288 {
		t.Fatalf("sizeofIn6Ifreq = %d, want 288 (macOS struct in6_ifreq)", sizeofIn6Ifreq)
	}

	// _IOW('i', 25, struct in6_ifreq) on macOS == 0x81206919.
	const wantNum = 0x81206919
	if siocDIFAddrInet6 != wantNum {
		t.Fatalf("siocDIFAddrInet6 = %#x, want %#x", siocDIFAddrInet6, wantNum)
	}

	// Sanity-check the derivation itself: same direction/group/number as SIOCDIFADDR, only the
	// embedded size differs.
	if got := (unix.SIOCDIFADDR & 0xe000ffff) | (uint(sizeofIn6Ifreq) << 16); got != wantNum {
		t.Fatalf("recomputed number = %#x, want %#x", got, wantNum)
	}

	// The kernel reads ifr_name then the sockaddr_in6 at the head of the union; these offsets
	// must match struct in6_ifreq (name at 0, addr right after IFNAMSIZ).
	if off := unsafe.Offsetof(in6Ifreq{}.name); off != 0 {
		t.Errorf("in6Ifreq.name offset = %d, want 0", off)
	}
	if off := unsafe.Offsetof(in6Ifreq{}.addr); off != uintptr(unix.IFNAMSIZ) {
		t.Errorf("in6Ifreq.addr offset = %d, want %d", off, unix.IFNAMSIZ)
	}
}
