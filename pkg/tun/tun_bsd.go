//go:build freebsd || openbsd

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	if cfg.Addr == "" && cfg.Addr6 == "" {
		err = errors.Errorf("ipv4 address and ipv6 address can not be empty at same time")
		return
	}

	var ipv4, ipv6 net.IP

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	var ifce tun.Device
	ifce, err = tun.CreateTUN("utun", mtu)
	if err != nil {
		err = errors.Wrap(err, "Failed to create TUN interface ")
		return
	}

	var name string
	name, err = ifce.Name()
	if err != nil {
		err = errors.Wrap(err, "Failed to get interface name ")
		return
	}

	if cfg.Addr != "" {
		ipv4, _, err = net.ParseCIDR(cfg.Addr6)
		if err != nil {
			err = errors.Wrap(err, "Failed to parse IPv6 CIDR ")
			return
		}
		cmd := fmt.Sprintf("ifconfig %s inet %s mtu %d up", ifce.Name(), cfg.Addr, mtu)
		log.Debugf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if err = exec.Command(args[0], args[1:]...).Run(); err != nil {
			err = errors.Errorf("%s: %v", cmd, err)
			return
		}
	}
	if cfg.Addr6 != "" {
		ipv6, _, err = net.ParseCIDR(cfg.Addr6)
		if err != nil {
			err = errors.Wrap(err, "Failed to parse IPv6 CIDR ")
			return
		}
		cmd := fmt.Sprintf("ifconfig %s add %s", ifce.Name(), cfg.Addr6)
		log.Debugf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if err = exec.Command(args[0], args[1:]...).Run(); err != nil {
			err = errors.Errorf("%s: %v", cmd, err)
			return
		}
	}

	if err = addTunRoutes(ifce.Name(), cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(ifce.Name())
	if err != nil {
		err = errors.Wrap(err, "Failed to get interface by name ")
		return
	}

	conn = &tunConn{
		ifce:  ifce,
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
		if route.Dst.IP.To4() != nil {
			cmd := fmt.Sprintf("route add -net %s -interface %s", route.Dst.String(), ifName)
		} else {
			cmd := fmt.Sprintf("route add -inet6 %s -interface %s", route.Dst.String(), ifName)
		}
		log.Debugf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return errors.Errorf("%s: %v", cmd, er)
		}
	}
	return nil
}
