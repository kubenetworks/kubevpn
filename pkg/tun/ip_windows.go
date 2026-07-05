//go:build windows

package tun

import (
	"fmt"
	"net"
	"net/netip"

	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// changeIP replaces the IP on an existing TUN device in place via winipcfg:
// delete the old address (if any) then add the new one. The wintun adapter and
// its file handle stay alive — only the interface's IP metadata changes.
func changeIP(ifName string, oldAddr, newAddr string) error {
	ifc, err := net.InterfaceByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}
	luid, err := winipcfg.LUIDFromIndex(uint32(ifc.Index))
	if err != nil {
		return fmt.Errorf("cannot resolve LUID for %s: %w", ifName, err)
	}

	if oldAddr != "" {
		if oldPrefix, perr := netip.ParsePrefix(oldAddr); perr == nil {
			_ = luid.DeleteIPAddress(oldPrefix)
		}
	}

	newPrefix, err := netip.ParsePrefix(newAddr)
	if err != nil {
		return fmt.Errorf("invalid new address %s: %w", newAddr, err)
	}
	if err = luid.AddIPAddress(newPrefix); err != nil {
		return fmt.Errorf("failed to add %s to %s: %w", newAddr, ifName, err)
	}
	return nil
}
