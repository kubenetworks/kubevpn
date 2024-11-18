//go:build windows

package tun

import (
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"syscall"
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
		err = fmt.Errorf("IPv4 address and IPv6 address can not be empty at same time")
		return
	}

	tunName := "KubeVPN"
	if len(cfg.Name) != 0 {
		tunName = cfg.Name
	}
	wireguardtun.WintunTunnelType = "KubeVPN"
	tunDevice, err := wireguardtun.CreateTUN(tunName, cfg.MTU)
	if err != nil {
		err = fmt.Errorf("failed to create TUN device: %w", err)
		return
	}

	ifUID := winipcfg.LUID(tunDevice.(*wireguardtun.NativeTun).LUID())

	var ipv4, ipv6 net.IP
	if cfg.Addr != "" {
		if ipv4, _, err = net.ParseCIDR(cfg.Addr); err != nil {
			return
		}
		var prefix netip.Prefix
		if prefix, err = netip.ParsePrefix(cfg.Addr); err != nil {
			return
		}
		if err = ifUID.AddIPAddress(prefix); err != nil {
			err = fmt.Errorf("can not setup IPv4 address %s to device %s : %v", prefix.String(), tunName, err)
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
		if err = ifUID.AddIPAddress(prefix); err != nil && !errors.Is(err, syscall.ERROR_NOT_FOUND) {
			err = fmt.Errorf("can not setup IPv6 address %s to device %s : %v", prefix.String(), tunName, err)
			return
		}
	}

	tunName, err = tunDevice.Name()
	if err != nil {
		return nil, nil, err
	}

	_ = ifUID.FlushRoutes(windows.AF_INET)
	_ = ifUID.FlushRoutes(windows.AF_INET6)

	if err = addTunRoutes(tunName /*cfg.Gateway,*/, cfg.Routes...); err != nil {
		return
	}
	var row *winipcfg.MibIfRow2
	if row, err = ifUID.Interface(); err != nil {
		return
	}
	if itf, err = net.InterfaceByIndex(int(row.InterfaceIndex)); err != nil {
		return
	}

	// windows,macOS,linux connect to same cluster
	// macOS and linux can ping each other, but macOS and linux can not ping windows
	var ipInterface *winipcfg.MibIPInterfaceRow
	ipInterface, err = ifUID.IPInterface(windows.AF_INET)
	if err != nil {
		return
	}
	ipInterface.ForwardingEnabled = true
	if err = ipInterface.Set(); err != nil {
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
	ifUID, err := winipcfg.LUIDFromIndex(uint32(name.Index))
	if err != nil {
		return err
	}
	for _, route := range routes {
		if route.Dst.String() == "" {
			continue
		}
		if route.GW == nil {
			if route.Dst.IP.To4() != nil {
				route.GW = net.IPv4zero
			} else {
				route.GW = net.IPv6zero
			}
		}
		var prefix netip.Prefix
		prefix, err = netip.ParsePrefix(route.Dst.String())
		if err != nil {
			return err
		}
		var addr netip.Addr
		addr, err = netip.ParseAddr(route.GW.String())
		if err != nil {
			return err
		}
		err = ifUID.AddRoute(prefix, addr.Unmap(), 0)
		if err != nil && !errors.Is(err, windows.ERROR_OBJECT_ALREADY_EXISTS) && !errors.Is(err, syscall.ERROR_NOT_FOUND) {
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
				log.Error(err)
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
