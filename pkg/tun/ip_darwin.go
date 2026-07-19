//go:build darwin || freebsd || openbsd

package tun

import (
	"fmt"
	"net/netip"
)

func changeIP(ifName string, oldAddr, newAddr string) error {
	prefix, err := netip.ParsePrefix(newAddr)
	if err != nil {
		return fmt.Errorf("invalid address %s: %w", newAddr, err)
	}
	// macOS SIOCAIFADDR only ADDS an alias, so the old address must be explicitly
	// removed first — otherwise the device ends up with both IPs. Best-effort.
	if oldAddr != "" {
		if oldPrefix, perr := netip.ParsePrefix(oldAddr); perr == nil && oldPrefix.Addr() != prefix.Addr() {
			_ = removeInterfaceAddress(ifName, oldPrefix.Addr())
		}
	}
	return setInterfaceAddress(ifName, prefix)
}
