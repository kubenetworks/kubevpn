//go:build freebsd || openbsd

package tun

import (
	"fmt"
	"net/netip"
	"os/exec"
)

// delInet6Address removes an IPv6 address via ifconfig on FreeBSD/OpenBSD. Their in6_ifreq
// struct size (which the SIOCDIFADDR_IN6 ioctl number embeds) differs from macOS's and from
// each other, so unlike the darwin build there is no single portable ioctl number to recompute;
// ifconfig is the reliable path here.
func delInet6Address(ifName string, ip netip.Addr) error {
	out, err := exec.Command("ifconfig", ifName, "inet6", ip.String(), "delete").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s inet6 %s delete: %w: %s", ifName, ip, err, out)
	}
	return nil
}
