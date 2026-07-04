//go:build windows

package dns

import (
	"context"
	"net"
	"net/netip"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func (c *Config) SetupDNS(ctx context.Context) error {
	clientConfig := c.Config
	tunName := c.TunName

	tun, err := net.InterfaceByName(tunName)
	if err != nil {
		return err
	}
	luid, err := winipcfg.LUIDFromIndex(uint32(tun.Index))
	if err != nil {
		return err
	}
	var servers []netip.Addr
	for _, s := range clientConfig.Servers {
		var addr netip.Addr
		addr, err = netip.ParseAddr(s)
		if err != nil {
			plog.G(ctx).Errorf("Parse %s failed: %s", s, err)
			return err
		}
		servers = append(servers, addr.Unmap())
	}
	err = luid.SetDNS(windows.AF_INET, servers, clientConfig.Search)
	if err != nil {
		plog.G(ctx).Errorf("Set DNS failed: %s", err)
		return err
	}
	err = luid.SetDNS(windows.AF_INET6, servers, clientConfig.Search)
	if err != nil {
		plog.G(ctx).Errorf("Set DNS failed: %s", err)
		return err
	}
	return nil
}

func (c *Config) CancelDNS() {
	_ = c.removeHosts()
	tun, err := net.InterfaceByName(c.TunName)
	if err != nil {
		return
	}
	luid, err := winipcfg.LUIDFromIndex(uint32(tun.Index))
	if err != nil {
		return
	}
	_ = luid.FlushDNS(windows.AF_INET)
	_ = luid.FlushDNS(windows.AF_INET6)
	_ = luid.FlushRoutes(windows.AF_INET)
	_ = luid.FlushRoutes(windows.AF_INET6)
}

func getHostFile() string {
	//return "/windows/system32/drivers/etc/hosts"
	return "C:\\Windows\\System32\\drivers\\etc\\hosts"
}
