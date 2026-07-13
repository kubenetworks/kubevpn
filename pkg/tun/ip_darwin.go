//go:build darwin || freebsd || openbsd

package tun

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func changeIP(ifName string, oldAddr, newAddr string) error {
	prefix, err := netip.ParsePrefix(newAddr)
	if err != nil {
		return fmt.Errorf("invalid address %s: %w", newAddr, err)
	}
	// macOS SIOCAIFADDR only ADDS an alias, so the old address must be explicitly
	// removed first — otherwise the device ends up with both IPs and macOS keeps
	// selecting the stale one as the outbound source. Best-effort.
	if oldAddr != "" {
		if oldPrefix, perr := netip.ParsePrefix(oldAddr); perr == nil && oldPrefix.Addr() != prefix.Addr() {
			if err = removeInterfaceAddress(ifName, oldPrefix.Addr()); err != nil {
				plog.G(context.Background()).Warnf("Failed to remove old TUN address %s from %s: %v", oldPrefix.Addr(), ifName, err)
			}
		}
	}
	if err = setInterfaceAddress(ifName, prefix); err != nil {
		return err
	}
	// Verify the old address is actually gone. A raw delete ioctl can return success without
	// removing a point-to-point alias; a stale address left here silently breaks outbound
	// traffic (the OS sources from it), so surface it loudly instead of failing invisibly.
	if oldAddr != "" {
		if oldPrefix, perr := netip.ParsePrefix(oldAddr); perr == nil && oldPrefix.Addr() != prefix.Addr() && interfaceHasAddr(ifName, oldPrefix.Addr()) {
			plog.G(context.Background()).Errorf("Old TUN address %s still present on %s after change to %s; outbound traffic may source from the stale address", oldPrefix.Addr(), ifName, prefix.Addr())
		}
	}
	return nil
}

// interfaceHasAddr reports whether ifName currently has ip assigned.
func interfaceHasAddr(ifName string, ip netip.Addr) bool {
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
