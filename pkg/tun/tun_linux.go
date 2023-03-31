//go:build linux

package tun

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/docker/libcontainer/netlink"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	if cfg.Addr == "" && cfg.Addr6 == "" {
		err = fmt.Errorf("ipv4 address and ipv6 address can not be empty at same time")
		return
	}

	var ipv4, ipv6 net.IP

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	var device tun.Device
	if device, err = tun.CreateTUN("utun", mtu); err != nil {
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
			err = fmt.Errorf("can not set ipv4 address %s to device %s : %v", ipv4.String(), name, err)
			return
		}
	}

	if cfg.Addr6 != "" {
		var ipv6CIDR *net.IPNet
		if ipv6, ipv6CIDR, err = net.ParseCIDR(cfg.Addr6); err != nil {
			return
		}
		if err = netlink.NetworkLinkAddIp(ifc, ipv6, ipv6CIDR); err != nil {
			err = fmt.Errorf("can not setup ipv6 address %s to device %s : %v", ipv6.String(), name, err)
			return
		}
	}

	if err = netlink.NetworkLinkUp(ifc); err != nil {
		err = fmt.Errorf("can not up device %s : %v", name, err)
		return
	}

	if err = os.Setenv(config.EnvTunNameOrLUID, name); err != nil {
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
		if route.Dst.String() == "" {
			continue
		}
		cmd := fmt.Sprintf("ip route add %s dev %s", route.Dst.String(), ifName)
		log.Debugf("[tun] %s", cmd)
		err := netlink.AddRoute(route.Dst.String(), "", "", ifName)
		if err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("%s: %v", cmd, err)
		}
	}
	return nil
}

func getInterface() (*net.Interface, error) {
	return net.InterfaceByName(os.Getenv(config.EnvTunNameOrLUID))
}
