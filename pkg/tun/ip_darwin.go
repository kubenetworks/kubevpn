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
	return setInterfaceAddress(ifName, prefix)
}
