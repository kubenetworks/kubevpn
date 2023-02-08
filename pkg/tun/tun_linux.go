//go:build linux && !windows && !darwin
// +build linux,!windows,!darwin

package tun

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/docker/libcontainer/netlink"
	"github.com/milosgajdos/tenus"
	log "github.com/sirupsen/logrus"
	"github.com/songgao/water"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	ip, ipNet, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return
	}

	ifce, err := water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: cfg.Name,
		},
	})
	if err != nil {
		return
	}

	link, err := tenus.NewLinkFrom(ifce.Name())
	if err != nil {
		return
	}

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	cmd := fmt.Sprintf("ip link set dev %s mtu %d", ifce.Name(), mtu)
	log.Debugf("[tun] %s", cmd)
	if er := link.SetLinkMTU(mtu); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	cmd = fmt.Sprintf("ip address add %s dev %s", cfg.Addr, ifce.Name())
	log.Debugf("[tun] %s", cmd)
	if er := link.SetLinkIp(ip, ipNet); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	cmd = fmt.Sprintf("ip link set dev %s up", ifce.Name())
	log.Debugf("[tun] %s", cmd)
	if er := link.SetLinkUp(); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	if err = os.Setenv(config.EnvTunNameOrLUID, ifce.Name()); err != nil {
		return nil, nil, err
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

func addTunRoutes(ifName string, routes ...types.Route) error {
	for _, route := range routes {
		if route.Dst.String() == "" {
			continue
		}
		cmd := fmt.Sprintf("ip route add %s dev %s", route.Dst.String(), ifName)
		log.Debugf("[tun] %s", cmd)
		if err := netlink.AddRoute(route.Dst.String(), "", "", ifName); err != nil && !errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("%s: %v", cmd, err)
		}
	}
	return nil
}
