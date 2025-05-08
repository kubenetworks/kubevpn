//go:build linux

package tun

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/docker/libcontainer/netlink"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	if cfg.Addr == "" && cfg.Addr6 == "" {
		err = fmt.Errorf("IPv4 address and IPv6 address can not be empty at same time")
		return
	}

	var ipv4, ipv6 net.IP

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	var interfaces []net.Interface
	interfaces, err = net.Interfaces()
	if err != nil {
		return
	}
	maxIndex := -1
	for _, i := range interfaces {
		ifIndex := -1
		_, err := fmt.Sscanf(i.Name, "utun%d", &ifIndex)
		if err == nil && ifIndex >= 0 && maxIndex < ifIndex {
			maxIndex = ifIndex
		}
	}
	var device tun.Device
	if device, err = tun.CreateTUN(fmt.Sprintf("utun%d", maxIndex+1), mtu); err != nil {
		return
	}

	var name string
	name, err = device.Name()
	if err != nil {
		return
	}
	var ifc *net.Interface
	if ifc, err = net.InterfaceByName(name); err != nil {
		err = fmt.Errorf("could not find interface name: %s", err)
		return
	}

	if err = netlink.NetworkSetMTU(ifc, mtu); err != nil {
		err = fmt.Errorf("can not setup mtu %d to device %s : %v", mtu, name, err)
		return
	}

	if cfg.Addr != "" {
		var ipv4CIDR *net.IPNet
		if ipv4, ipv4CIDR, err = net.ParseCIDR(cfg.Addr); err != nil {
			return
		}
		if err = netlink.NetworkLinkAddIp(ifc, ipv4, ipv4CIDR); err != nil {
			err = fmt.Errorf("can not set IPv4 address %s to device %s : %v", ipv4.String(), name, err)
			return
		}
	}

	if cfg.Addr6 != "" {
		var ipv6CIDR *net.IPNet
		if ipv6, ipv6CIDR, err = net.ParseCIDR(cfg.Addr6); err != nil {
			return
		}
		if err = netlink.NetworkLinkAddIp(ifc, ipv6, ipv6CIDR); err != nil && !errors.Is(err, syscall.ENOTSUP) {
			err = fmt.Errorf("can not setup IPv6 address %s to device %s : %v", ipv6.String(), name, err)
			return
		}
	}

	if err = netlink.NetworkLinkUp(ifc); err != nil {
		err = fmt.Errorf("can not up device %s : %v", name, err)
		return
	}

	if err = addTunRoutes(name, cfg.Routes...); err != nil {
		return
	}

	if itf, err = net.InterfaceByName(name); err != nil {
		return
	}

	conn = &tunConn{
		ifce:  device,
		addr:  &net.IPAddr{IP: ipv4},
		addr6: &net.IPAddr{IP: ipv6},
	}
	return
}

func addTunRoutes(ifName string, routes ...types.Route) error {
	for _, route := range routes {
		if net.ParseIP(route.Dst.IP.String()) == nil {
			continue
		}
		// ip route add 192.168.1.123/32 dev utun0
		err := netlink.AddRoute(route.Dst.String(), "", "", ifName)
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("failed to add route: %v", err)
		}
	}
	return nil
}
