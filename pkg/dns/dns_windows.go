//go:build windows

package dns

import (
	"fmt"
	"net"
	"net/netip"
	"os/exec"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func (c *Config) SetupDNS() error {
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
			log.Errorf("parse %s failed: %s", s, err)
			return err
		}
		servers = append(servers, addr.Unmap())
	}
	err = luid.SetDNS(windows.AF_INET, servers, clientConfig.Search)
	if err != nil {
		log.Errorf("set DNS failed: %s", err)
		return err
	}
	err = luid.SetDNS(windows.AF_INET6, servers, clientConfig.Search)
	if err != nil {
		log.Errorf("set DNS failed: %s", err)
		return err
	}
	//_ = updateNicMetric(tunName)
	//_ = addNicSuffixSearchList(clientConfig.Search)
	return nil
}

func (c *Config) CancelDNS() {
	c.removeHosts(c.Hosts)
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

func updateNicMetric(name string) error {
	cmd := exec.Command("PowerShell", []string{
		"Set-NetIPInterface",
		"-InterfaceAlias",
		fmt.Sprintf("\"%s\"", name),
		"-InterfaceMetric",
		"1",
	}...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warnf("error while update nic metrics, error: %v, output: %s, command: %v", err, string(out), cmd.Args)
	}
	return err
}

// @see https://docs.microsoft.com/en-us/powershell/module/dnsclient/set-dnsclientglobalsetting?view=windowsserver2019-ps#example-1--set-the-dns-suffix-search-list
func addNicSuffixSearchList(search []string) error {
	cmd := exec.Command("PowerShell", []string{
		"Set-DnsClientGlobalSetting",
		"-SuffixSearchList",
		fmt.Sprintf("@(\"%s\", \"%s\", \"%s\")", search[0], search[1], search[2]),
	}...)
	output, err := cmd.CombinedOutput()
	log.Debugln(cmd.Args)
	if err != nil {
		log.Warnf("error while set dns suffix search list, err: %v, output: %s, command: %v", err, string(output), cmd.Args)
	}
	return err
}

func GetHostFile() string {
	//return "/windows/system32/drivers/etc/hosts"
	return "C:\\Windows\\System32\\drivers\\etc\\hosts"
}
