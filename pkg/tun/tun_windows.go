//go:build windows

package tun

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	wintun "golang.zx2c4.com/wintun"
	wireguardtun "golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
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
	var interfaces []net.Interface
	interfaces, err = net.Interfaces()
	if err != nil {
		return
	}
	maxIndex := -1
	for _, i := range interfaces {
		ifIndex := -1
		_, err := fmt.Sscanf(i.Name, "KubeVPN%d", &ifIndex)
		if err == nil && ifIndex >= 0 && maxIndex < ifIndex {
			maxIndex = ifIndex
		}
	}
	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = config.DefaultMTU
	}

	wireguardtun.WintunTunnelType = "KubeVPN"
	tunDevice, err := wireguardtun.CreateTUN(fmt.Sprintf("KubeVPN%d", maxIndex+1), mtu)
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
	if cfg.Addr != "" {
		var ipInterface *winipcfg.MibIPInterfaceRow
		ipInterface, err = ifUID.IPInterface(windows.AF_INET)
		if err != nil {
			return
		}
		ipInterface.ForwardingEnabled = true
		ipInterface.NLMTU = uint32(mtu)
		if err = ipInterface.Set(); err != nil {
			return
		}
	}
	if cfg.Addr6 != "" {
		var ipInterface *winipcfg.MibIPInterfaceRow
		ipInterface, err = ifUID.IPInterface(windows.AF_INET6)
		if err != nil {
			return
		}
		ipInterface.ForwardingEnabled = true
		ipInterface.NLMTU = uint32(mtu)
		if err = ipInterface.Set(); err != nil {
			return
		}
	}

	conn = &winTunConn{ifce: tunDevice, addr: &net.IPAddr{IP: ipv4}, addr6: &net.IPAddr{IP: ipv6}}
	return
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
				plog.G(context.Background()).Error(err)
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
	if len(b) == 0 {
		return 0 /*errors.New("can not write empty buffer")*/, nil
	}
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
