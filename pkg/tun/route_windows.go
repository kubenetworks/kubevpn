//go:build windows

package tun

import (
	"net"
	"net/netip"
	"syscall"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func addTunRoutes(tunName string, routes ...types.Route) error {
	name, err2 := net.InterfaceByName(tunName)
	if err2 != nil {
		return err2
	}
	ifUID, err := winipcfg.LUIDFromIndex(uint32(name.Index))
	if err != nil {
		return err
	}
	for _, route := range routes {
		if net.ParseIP(route.Dst.IP.String()) == nil {
			continue
		}
		if route.GW == nil {
			if route.Dst.IP.To4() != nil {
				route.GW = net.IPv4zero
			} else {
				route.GW = net.IPv6zero
			}
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(route.Dst.String())
		if err != nil {
			return err
		}
		var addr netip.Addr
		addr, err = netip.ParseAddr(route.GW.String())
		if err != nil {
			return err
		}
		err = ifUID.AddRoute(prefix, addr.Unmap(), 0)
		if err != nil && !errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) && !errors.Is(err, syscall.ERROR_NOT_FOUND) {
			return err
		}
	}
	return nil
}

func deleteTunRoutes(tunName string, routes ...types.Route) error {
	name, err2 := net.InterfaceByName(tunName)
	if err2 != nil {
		return err2
	}
	ifUID, err := winipcfg.LUIDFromIndex(uint32(name.Index))
	if err != nil {
		return err
	}
	for _, route := range routes {
		if net.ParseIP(route.Dst.IP.String()) == nil {
			continue
		}
		if route.GW == nil {
			if route.Dst.IP.To4() != nil {
				route.GW = net.IPv4zero
			} else {
				route.GW = net.IPv6zero
			}
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(route.Dst.String())
		if err != nil {
			return err
		}
		var addr netip.Addr
		addr, err = netip.ParseAddr(route.GW.String())
		if err != nil {
			return err
		}
		err = ifUID.DeleteRoute(prefix, addr.Unmap())
		if err != nil && !errors.Is(err, syscall.ERROR_NOT_FOUND) {
			return err
		}
	}
	return nil
}
