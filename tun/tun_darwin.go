package tun

import (
	"fmt"
	"kubevpn/util"
	"net"
	"os/exec"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/songgao/water"
)

func createTun(cfg TunConfig) (conn net.Conn, itf *net.Interface, err error) {
	ip, _, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return
	}

	ifce, err := water.New(water.Config{
		DeviceType: water.TUN,
	})
	if err != nil {
		return
	}

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = util.DefaultMTU
	}

	peer := cfg.Peer
	if peer == "" {
		peer = ip.String()
	}
	cmd := fmt.Sprintf("ifconfig %s inet %s %s mtu %d up",
		ifce.Name(), cfg.Addr, peer, mtu)
	log.Debug("[tun]", cmd)
	args := strings.Split(cmd, " ")
	if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	if err = addTunRoutes(ifce.Name(), cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(ifce.Name())
	if err != nil {
		return
	}

	conn = &tunConn{
		ifce: ifce,
		addr: &net.IPAddr{IP: ip},
	}
	return
}

func addTunRoutes(ifName string, routes ...IPRoute) error {
	for _, route := range routes {
		if route.Dest == nil {
			continue
		}
		cmd := fmt.Sprintf("route add -net %s -interface %s", route.Dest.String(), ifName)
		log.Debug("[tun]", cmd)
		args := strings.Split(cmd, " ")
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return fmt.Errorf("%s: %v", cmd, er)
		}
	}
	return nil
}
