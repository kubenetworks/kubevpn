//go:build windows

package tun

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"reflect"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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

	isIPv6Enable, _ := isIPv6Enabled()
	if cfg.Addr6 != "" && isIPv6Enable {
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
		err = ifName.AddRoute(prefix, addr.Unmap(), 0)
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

func (c *winTunConn) Read(b []byte) (int, error) {
	sizes := make([]int, 1)
	num, err := c.ifce.Read([][]byte{b}, sizes, 0)
	if err != nil {
		if errors.Is(err, wireguardtun.ErrTooManySegments) {
			log.Errorf("Dropped some packets from multi-segment read: %v", err)
			return 0, nil
		}
		if !errors.Is(err, os.ErrClosed) {
			log.Errorf("Failed to read packet from TUN device: %v", err)
			return 0, nil
		}
		return 0, err
	}
	if num == 0 {
		return 0, nil
	}
	return sizes[0], nil
}

func (c *winTunConn) Write(b []byte) (int, error) {
	n, err := c.ifce.Write([][]byte{b}, 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return len(b), nil
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

/*
*
Reference: https://learn.microsoft.com/en-us/troubleshoot/windows-server/networking/configure-ipv6-in-windows#use-registry-key-to-configure-ipv6

| IPv6 Functionality                                 | Registry value and comments                                                                   |
|----------------------------------------------------|-----------------------------------------------------------------------------------------------|
| Prefer IPv4 over IPv6                              | Decimal 32<br>Hexadecimal 0x20<br>Binary xx1x xxxx<br>Recommended instead of disabling IPv6.  |
| Disable IPv6                                       | Decimal 255<br>Hexadecimal 0xFF<br>Binary 1111 1111<br>										 |
| Disable IPv6 on all nontunnel interfaces           | Decimal 16<br>Hexadecimal 0x10<br>Binary xxx1 xxxx                                            |
| Disable IPv6 on all tunnel interfaces              | Decimal 1<br>Hexadecimal 0x01<br>Binary xxxx xxx1                                             |
| Disable IPv6 on all nontunnel interfaces (except the loopback) and on IPv6 tunnel interface | Decimal 17<br>Hexadecimal 0x11<br>Binary xxx1 xxx1   |
| Prefer IPv6 over IPv4                              | Binary xx0x xxxx                                                                              |
| Re-enable IPv6 on all nontunnel interfaces         | Binary xxx0 xxxx                                                                              |
| Re-enable IPv6 on all tunnel interfaces            | Binary xxx xxx0                                                                               |
| Re-enable IPv6 on nontunnel interfaces and on IPv6 tunnel interfaces | Binary xxx0 xxx0                                                            |

Enable IPv6:

	  Default Value 		Hexadecimal 0x00
							Decimal 0
	  Prefer IPv4 over IPv6	Hexadecimal 0x20
	  						Decimal 32
	  Prefer IPv6 over IPv4	Binary xx0x xxxx
*/
func isIPv6Enabled() (bool, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\Tcpip6\Parameters`, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	val, valtype, err := key.GetIntegerValue("DisabledComponents")
	if errors.Is(err, registry.ErrNotExist) {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	if valtype != registry.DWORD {
		return false, nil
	}

	switch val {
	case 0x00:
		return true, nil
	case 0x20:
		return true, nil
	default:
		return false, nil
	}
}
