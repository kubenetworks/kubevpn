//go:build linux

package tun

import (
	"fmt"
	"net"

	"github.com/docker/libcontainer/netlink"
)

func changeIP(ifName string, oldAddr, newAddr string) error {
	ifc, err := net.InterfaceByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s not found: %w", ifName, err)
	}

	newIP, newCIDR, err := net.ParseCIDR(newAddr)
	if err != nil {
		return fmt.Errorf("invalid new address %s: %w", newAddr, err)
	}
	// Add before delete. Linux flushes a device's entire IPv4 route table the moment it loses
	// its last IPv4 address, so deleting first leaves a window with no address, and the cluster
	// routes are gone for good when the new one arrives — worse, traffic for those destinations
	// falls back to the default route and leaves over the physical NIC in the meantime.
	// Overlapping the two addresses for an instant keeps the device addressed throughout.
	// See docs/52-stale-route-src-after-ip-change.md.
	if err = netlink.NetworkLinkAddIp(ifc, newIP, newCIDR); err != nil {
		return fmt.Errorf("failed to add %s to %s: %w", newAddr, ifName, err)
	}

	if oldAddr != "" {
		oldIP, oldCIDR, parseErr := net.ParseCIDR(oldAddr)
		if parseErr == nil && oldIP != nil {
			_ = netlink.NetworkLinkDelIp(ifc, oldIP, oldCIDR)
		}
	}
	return nil
}
