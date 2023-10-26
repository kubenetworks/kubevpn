//go:build windows

package tun

import (
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	wintun "golang.zx2c4.com/wintun"
	wireguardtun "golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func createTun(cfg Config) (conn net.Conn, itf *net.Interface, err error) {
	if cfg.Addr == "" && cfg.Addr6 == "" {
		err = fmt.Errorf("ipv4 address and ipv6 address can not be empty at same time")
		return
	}

	interfaceName := "kubevpn"
	if len(cfg.Name) != 0 {
		interfaceName = cfg.Name
	}
	tunDevice, err := wireguardtun.CreateTUN(interfaceName, cfg.MTU)
	if err != nil {
		err = fmt.Errorf("failed to create TUN device: %w", err)
		return
	}

	ifName := winipcfg.LUID(tunDevice.(*wireguardtun.NativeTun).LUID())

	var ipv4, ipv6 net.IP
	if cfg.Addr != "" {
		if ipv4, _, err = net.ParseCIDR(cfg.Addr); err != nil {
			return
		}
		var prefix netip.Prefix
		if prefix, err = netip.ParsePrefix(cfg.Addr); err != nil {
			return
		}
		if err = ifName.AddIPAddress(prefix); err != nil {
			return
		}
	}

	if cfg.Addr6 != "" {
		if ipv6, _, err = net.ParseCIDR(cfg.Addr6); err != nil {
			return
		}
		var prefix netip.Prefix
		if prefix, err = netip.ParsePrefix(cfg.Addr6); err != nil {
			return
		}
		if err = ifName.AddIPAddress(prefix); err != nil {
			return
		}
	}

	var tunName string
	tunName, err = tunDevice.Name()
	if err != nil {
		return nil, nil, err
	}

	_ = ifName.FlushRoutes(windows.AF_INET)
	_ = ifName.FlushRoutes(windows.AF_INET6)

	if err = addTunRoutes(tunName /*cfg.Gateway,*/, cfg.Routes...); err != nil {
		return
	}
	var row *winipcfg.MibIfRow2
	if row, err = ifName.Interface(); err != nil {
		return
	}
	if itf, err = net.InterfaceByIndex(int(row.InterfaceIndex)); err != nil {
		return
	}
	conn = &winTunConn{ifce: tunDevice, addr: &net.IPAddr{IP: ipv4}, addr6: &net.IPAddr{IP: ipv6}}
	return
}

func addTunRoutes(tunName string, routes ...types.Route) error {
	name, err2 := net.InterfaceByName(tunName)
	if err2 != nil {
		return err2
	}
	ifName, err := winipcfg.LUIDFromIndex(uint32(name.Index))
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Dst.String() == "" {
			continue
		}
		var gw string
		if gw != "" {
			route.GW = net.ParseIP(gw)
		} else {
			if route.Dst.IP.To4() != nil {
				route.GW = net.IPv4zero
			} else {
				route.GW = net.IPv6zero
			}
		}
		prefix, err := netip.ParsePrefix(route.Dst.String())
		if err != nil {
			return err
		}
		var addr netip.Addr
		addr, err = netip.ParseAddr(route.GW.String())
		if err != nil {
			return err
		}
		err = ifName.AddRoute(prefix, addr, 0)
		if err != nil && err != windows.ERROR_OBJECT_ALREADY_EXISTS {
			return err
		}
	}
	return nil
}

type winTunConn struct {
	ifce  wireguardtun.Device
	addr  net.Addr
	addr6 net.Addr
}

func (c *winTunConn) Close() error {
	err := c.ifce.Close()
	wintun.Uninstall()
	defer func() {
		defer func() {
			if err := recover(); err != nil {
				log.Debug(err)
			}
		}()
		tun := c.ifce.(*wireguardtun.NativeTun)
		v := reflect.ValueOf(tun).Elem().FieldByName("wt")
		vv := reflect.Indirect(v).FieldByName("handle")
		err = windows.FreeLibrary(windows.Handle(vv.Uint()))
	}()
	return err
}

func (c *winTunConn) Read(b []byte) (n int, err error) {
	return c.ifce.Read(b, 0)
}

func (c *winTunConn) Write(b []byte) (n int, err error) {
	return c.ifce.Write(b, 0)
}

func (c *winTunConn) LocalAddr() net.Addr {
	return c.addr
}

func (c *winTunConn) RemoteAddr() net.Addr {
	return &net.IPAddr{}
}

func (c *winTunConn) SetDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *winTunConn) SetReadDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}

func (c *winTunConn) SetWriteDeadline(time.Time) error {
	return &net.OpError{Op: "set", Net: "tun", Source: nil, Addr: nil, Err: errors.New("deadline not supported")}
}
