//go:build darwin

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
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
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

	// set ipv4 address
	if ipv4, _, err = net.ParseCIDR(cfg.Addr); err != nil {
		return
	}
	setIPv4Cmd := fmt.Sprintf("ifconfig %s inet %s %s mtu %d up", name, cfg.Addr, ipv4.String(), mtu)
	log.Debugf("[tun] %s", setIPv4Cmd)
	args := strings.Split(setIPv4Cmd, " ")
	if err = exec.Command(args[0], args[1:]...).Run(); err != nil {
		err = errors.Errorf("%s: %v", setIPv4Cmd, err)
		return
	}

	// set ipv6 address
	if cfg.Addr6 != "" {
		var ipv6CIDR *net.IPNet
		if ipv6, ipv6CIDR, err = net.ParseCIDR(cfg.Addr6); err != nil {
			return
		}
		ones, _ := ipv6CIDR.Mask.Size()
		setIPv6Cmd := fmt.Sprintf("ifconfig %s inet6 %s prefixlen %d alias", name, ipv6.String(), ones)
		log.Debugf("[tun] %s", setIPv6Cmd)
		args = strings.Split(setIPv6Cmd, " ")
		if err = exec.Command(args[0], args[1:]...).Run(); err != nil {
			err = errors.Errorf("%s: %v", setIPv6Cmd, err)
			return
		}
	}

	if err = addTunRoutes(name, cfg.Routes...); err != nil {
		errors.LogErrorf("add tun routes failed: %v", err)
		return
	}

	if itf, err = net.InterfaceByName(name); err != nil {
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
		var cmd string
		// ipv4
		if route.Dst.IP.To4() != nil {
			cmd = fmt.Sprintf("route add -net %s -interface %s", route.Dst.String(), ifName)
		} else { // ipv6
			cmd = fmt.Sprintf("route add -inet6 %s -interface %s", route.Dst.String(), ifName)
		}
		log.Debugf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		err := exec.Command(args[0], args[1:]...).Run()
		if err != nil {
			return errors.Errorf("run cmd %s: %v", cmd, err)
		}
	}
	return nil
}
