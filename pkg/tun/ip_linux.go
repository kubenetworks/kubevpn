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

	if oldAddr != "" {
		oldIP, oldCIDR, parseErr := net.ParseCIDR(oldAddr)
		if parseErr == nil && oldIP != nil {
			_ = netlink.NetworkLinkDelIp(ifc, oldIP, oldCIDR)
		}
	}

	newIP, newCIDR, err := net.ParseCIDR(newAddr)
	if err != nil {
		return fmt.Errorf("invalid new address %s: %w", newAddr, err)
	}
	if err = netlink.NetworkLinkAddIp(ifc, newIP, newCIDR); err != nil {
		return fmt.Errorf("failed to add %s to %s: %w", newAddr, ifName, err)
	}
	return nil
}
