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
		err = fmt.Errorf("ipv4 address and ipv6 address can not be empty at same time")
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

	var ifce tun.Device
	ifce, err = tun.CreateTUN(fmt.Sprintf("utun%d", maxIndex+1), mtu)
	if err != nil {
		return
	}

	var name string
	name, err = ifce.Name()
	if err != nil {
		return
	}

	if cfg.Addr != "" {
		ipv4, _, err = net.ParseCIDR(cfg.Addr6)
		if err != nil {
			return
		}
		cmd := fmt.Sprintf("ifconfig %s inet %s mtu %d up", ifce.Name(), cfg.Addr, mtu)
		log.Debugf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if err = exec.Command(args[0], args[1:]...).Run(); err != nil {
			err = fmt.Errorf("%s: %v", cmd, err)
			return
		}
	}
	if cfg.Addr6 != "" {
		ipv6, _, err = net.ParseCIDR(cfg.Addr6)
		if err != nil {
			return
		}
		cmd := fmt.Sprintf("ifconfig %s add %s", ifce.Name(), cfg.Addr6)
		log.Debugf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if err = exec.Command(args[0], args[1:]...).Run(); err != nil {
			err = fmt.Errorf("%s: %v", cmd, err)
			return
		}
	}

	if err = addTunRoutes(ifce.Name(), cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(ifce.Name())
	if err != nil {
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
			return fmt.Errorf("%s: %v", cmd, er)
		}
	}
	return nil
}
