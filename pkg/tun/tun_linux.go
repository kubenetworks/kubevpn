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
	"github.com/milosgajdos/tenus"
	log "github.com/sirupsen/logrus"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	ip, ipNet, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return
	}

	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	var ifce tun.Device
	ifce, err = tun.CreateTUN("utun", mtu)
	if err != nil {
		return
	}

	var name string
	name, err = ifce.Name()
	if err != nil {
		return
	}

	link, err := tenus.NewLinkFrom(name)
	if err != nil {
		return
	}

	cmd := fmt.Sprintf("ip link set dev %s mtu %d", name, mtu)
	log.Debugf("[tun] %s", cmd)
	if er := link.SetLinkMTU(mtu); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	cmd = fmt.Sprintf("ip address add %s dev %s", cfg.Addr, name)
	log.Debugf("[tun] %s", cmd)
	if er := link.SetLinkIp(ip, ipNet); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	cmd = fmt.Sprintf("ip link set dev %s up", name)
	log.Debugf("[tun] %s", cmd)
	if er := link.SetLinkUp(); er != nil {
		err = fmt.Errorf("%s: %v", cmd, er)
		return
	}

	if err = os.Setenv(config.EnvTunNameOrLUID, name); err != nil {
		return nil, nil, err
	}

	if err = addTunRoutes(name, cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(name)
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

func getInterface() (*net.Interface, error) {
	return net.InterfaceByName(os.Getenv(config.EnvTunNameOrLUID))
}
