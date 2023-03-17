//go:build windows

package tun

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	wintun "golang.zx2c4.com/wintun"
	wireguardtun "golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

func createTun(cfg Config) (net.Conn, *net.Interface, error) {
	ip, _, err := net.ParseCIDR(cfg.Addr)
	if err != nil {
		return nil, nil, err
	}
	interfaceName := "wg1"
	if len(cfg.Name) != 0 {
		interfaceName = cfg.Name
	}
	tunDevice, err := wireguardtun.CreateTUN(interfaceName, cfg.MTU)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create TUN device: %w", err)
	}

	ifName := winipcfg.LUID(tunDevice.(*wireguardtun.NativeTun).LUID())

	var prefix netip.Prefix
	prefix, err = netip.ParsePrefix(cfg.Addr)
	if err != nil {
		return nil, nil, err
	}

	if err = ifName.AddIPAddress(prefix); err != nil {
		return nil, nil, err
	}

	luid := fmt.Sprintf("%d", tunDevice.(*wireguardtun.NativeTun).LUID())
	if err = os.Setenv(config.EnvTunNameOrLUID, luid); err != nil {
		return nil, nil, err
	}
	_ = ifName.FlushRoutes(windows.AF_INET)
	if err = addTunRoutes(luid /*cfg.Gateway,*/, cfg.Routes...); err != nil {
		return nil, nil, err
	}

	row, _ := ifName.Interface()
	iface, _ := net.InterfaceByIndex(int(row.InterfaceIndex))
	return &winTunConn{ifce: tunDevice, addr: &net.IPAddr{IP: ip}}, iface, nil
}

func addTunRoutes(luid string, routes ...types.Route) error {
	parseUint, err := strconv.ParseUint(luid, 10, 64)
	if err != nil {
		return err
	}
	ifName := winipcfg.LUID(parseUint)
	for _, route := range routes {
		if route.Dst.String() == "" {
			continue
		}
		var gw string
		if gw != "" {
			route.GW = net.ParseIP(gw)
		} else {
			route.GW = net.IPv4(0, 0, 0, 0)
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
	ifce wireguardtun.Device
	addr net.Addr
}

func (c *winTunConn) Close() error {
	err := c.ifce.Close()
	//if name, err := c.ifce.Name(); err == nil {
	//	if wt, err := wireguardtun.WintunPool.OpenAdapter(name); err == nil {
	//		_, err = wt.Delete(true)
	//	}
	//}
	wintun.Uninstall()
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

func getInterface() (*net.Interface, error) {
	env := os.Getenv(config.EnvTunNameOrLUID)
	parseUint, err := strconv.ParseUint(env, 10, 64)
	if err != nil {
		return nil, err
	}
	ifRow, err := winipcfg.LUID(parseUint).Interface()
	if err != nil {
		return nil, err
	}
	iface, err := net.InterfaceByIndex(int(ifRow.InterfaceIndex))
	if err != nil {
		return nil, err
	}
	return iface, nil
}
