package core

import (
	"context"
	"fmt"
	"github.com/pkg/errors"
	"k8s.io/client-go/util/retry"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/go-log/log"
	"github.com/songgao/water"
)

func createTun(cfg TunConfig) (conn net.Conn, itf *net.Interface, err error) {
	ip, ipNet, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return
	}
	ifce, err := OpenTun(context.Background())
	if err != nil {
		return
	}

	cmd := fmt.Sprintf("netsh interface ip set address name=\"%s\" "+
		"source=static addr=%s mask=%s gateway=none",
		ifce.Name(), ip.String(), ipMask(ipNet.Mask))
	log.Log("[tun]", cmd)

	args := strings.Split(cmd, " ")
	err = retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return fmt.Errorf("%s: %v", cmd, er)
		}
		return nil
	})
	if err != nil {
		return
	}

	if err = addTunRoutes(ifce.Name(), cfg.Gateway, cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(ifce.Name())
	if err != nil {
		return
	}

	conn = &WinTunConn{
		ifce: ifce,
		addr: &net.IPAddr{IP: ip},
	}
	return
}

type WinTunConn struct {
	ifce *Device
	addr net.Addr
}

func (c *WinTunConn) Read(b []byte) (n int, err error) {
	return c.ifce.Read(b, 0)
}

func (c *WinTunConn) Write(b []byte) (n int, err error) {
	return c.ifce.Write(b, 0)
}

func (c *WinTunConn) Close() (err error) {
	return c.ifce.Close()
}

func (c *WinTunConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *WinTunConn) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (c *WinTunConn) SetDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tuntap", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *WinTunConn) SetReadDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tuntap", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *WinTunConn) SetWriteDeadline(t time.Time) error {
	return &net.OpError{Op: "set", Net: "tuntap", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func createTap(cfg TapConfig) (conn net.Conn, itf *net.Interface, err error) {
	ip, ipNet, _ := net.ParseCIDR(cfg.Addr)

	ifce, err := water.New(water.Config{
		DeviceType: water.TAP,
		PlatformSpecificParams: water.PlatformSpecificParams{
			ComponentID:   "tap0901",
			InterfaceName: cfg.Name,
			Network:       cfg.Addr,
		},
	})
	if err != nil {
		return
	}

	if ip != nil && ipNet != nil {
		cmd := fmt.Sprintf("netsh interface ip set address name=\"%s\" "+
			"source=static addr=%s mask=%s gateway=none",
			ifce.Name(), ip.String(), ipMask(ipNet.Mask))
		log.Log("[tap]", cmd)
		args := strings.Split(cmd, " ")
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			err = fmt.Errorf("%s: %v", cmd, er)
			return
		}
	}

	if err = addTapRoutes(ifce.Name(), cfg.Gateway, cfg.Routes...); err != nil {
		return
	}

	itf, err = net.InterfaceByName(ifce.Name())
	if err != nil {
		return
	}

	conn = &tunTapConn{
		ifce: ifce,
		addr: &net.IPAddr{IP: ip},
	}
	return
}

func addTunRoutes(ifName string, gw string, routes ...IPRoute) error {
	for _, route := range routes {
		if route.Dest == nil {
			continue
		}

		deleteRoute(ifName, route.Dest.String())

		cmd := fmt.Sprintf("netsh interface ip add route prefix=%s interface=\"%s\" store=active",
			route.Dest.String(), ifName)
		if gw != "" {
			cmd += " nexthop=" + gw
		}
		log.Logf("[tun] %s", cmd)
		args := strings.Split(cmd, " ")
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return fmt.Errorf("%s: %v", cmd, er)
		}
	}
	return nil
}

func addTapRoutes(ifName string, gw string, routes ...string) error {
	for _, route := range routes {
		if route == "" {
			continue
		}

		deleteRoute(ifName, route)

		cmd := fmt.Sprintf("netsh interface ip add route prefix=%s interface=\"%s\" store=active",
			route, ifName)
		if gw != "" {
			cmd += " nexthop=" + gw
		}
		log.Logf("[tap] %s", cmd)
		args := strings.Split(cmd, " ")
		if er := exec.Command(args[0], args[1:]...).Run(); er != nil {
			return fmt.Errorf("%s: %v", cmd, er)
		}
	}
	return nil
}

func deleteRoute(ifName string, route string) error {
	cmd := fmt.Sprintf("netsh interface ip delete route prefix=%s interface=\"%s\" store=active",
		route, ifName)
	args := strings.Split(cmd, " ")
	return exec.Command(args[0], args[1:]...).Run()
}

func ipMask(mask net.IPMask) string {
	return fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
}
