//go:build linux

package tun

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/docker/libcontainer/netlink"
	netlink1 "github.com/vishvananda/netlink"
)

func addTunRoutes(ifName string, routes ...types.Route) error {
	for _, route := range routes {
		if net.ParseIP(route.Dst.IP.String()) == nil {
			continue
		}
		// ip route add 192.168.1.123/32 dev utun0
		err := netlink.AddRoute(route.Dst.String(), "", "", ifName)
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add dst %v via %s to route table: %v", route.Dst.String(), ifName, err)
		}
	}
	return nil
}

func deleteTunRoutes(ifName string, routes ...types.Route) error {
	tunIfi, err := net.InterfaceByName(ifName)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if net.ParseIP(route.Dst.IP.String()) == nil {
			continue
		}
		// ip route add 192.168.1.123/32 dev utun0
		err = netlink1.RouteDel(&netlink1.Route{
			Dst:       &route.Dst,
			LinkIndex: tunIfi.Index,
		})
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add dst %v via %s to route table: %v", route.Dst.String(), ifName, err)
		}
	}
	return nil
}
