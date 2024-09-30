//go:build linux

package dns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"

	"github.com/coredns/caddy"
	_ "github.com/coredns/coredns/core/dnsserver"
	_ "github.com/coredns/coredns/core/plugin"
	"github.com/docker/docker/libnetwork/resolvconf"
	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	"tailscale.com/net/dns"
	"tailscale.com/util/dnsname"
)

// SetupDNS
// systemd-resolve --status, systemd-resolve --flush-caches
func (c *Config) SetupDNS(ctx context.Context) error {
	config := c.Config
	tunName := c.TunName

	// TODO consider use https://wiki.debian.org/NetworkManager and nmcli to config DNS
	// try to solve:
	// sudo systemd-resolve --set-dns 172.28.64.10 --interface tun0 --set-domain=vke-system.svc.cluster.local --set-domain=svc.cluster.local --set-domain=cluster.local
	//Failed to set DNS configuration: Unit dbus-org.freedesktop.resolve1.service not found.
	// ref: https://superuser.com/questions/1427311/activation-via-systemd-failed-for-unit-dbus-org-freedesktop-resolve1-service
	// systemctl enable systemd-resolved.service
	_ = exec.Command("systemctl", "enable", "systemd-resolved.service").Run()
	// systemctl start systemd-resolved.service
	_ = exec.Command("systemctl", "start", "systemd-resolved.service").Run()
	//systemctl status systemd-resolved.service
	_ = exec.Command("systemctl", "status", "systemd-resolved.service").Run()

	var exists = func(cmd string) bool {
		_, err := exec.LookPath(cmd)
		return err == nil
	}

	if exists("resolvectl") {
		_ = GetResolveCtlCmd(ctx, tunName, config)
	}
	if exists("systemd-resolve") {
		_ = GetSystemdResolveCmd(ctx, tunName, config)
	}
	_ = c.UseLibraryDNS(tunName, config)

	filename := resolvconf.Path()
	readFile, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	localResolvConf, err := miekgdns.ClientConfigFromReader(bytes.NewBufferString(string(readFile)))
	if err != nil {
		return err
	}
	localResolvConf.Servers = append([]string{config.Servers[0]}, localResolvConf.Servers...)

	err = WriteResolvConf(resolvconf.Path(), *localResolvConf)
	return err
}

// GetResolveCtlCmd
// resolvectl dns utun0 10.10.129.161
// resolvectl domain utun0 default.svc.cluster.local svc.cluster.local cluster.local
func GetResolveCtlCmd(ctx context.Context, tunName string, config *miekgdns.ClientConfig) error {
	cmd := exec.CommandContext(ctx, "resolvectl", "dns", tunName, config.Servers[0])
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Debugf("Failed to exec cmd '%s': %s", strings.Join(cmd.Args, " "), string(output))
		return err
	}
	cmd = exec.CommandContext(ctx, "resolvectl", "domain", tunName, config.Search[0], config.Search[1], config.Search[2])
	output, err = cmd.CombinedOutput()
	if err != nil {
		log.Debugf("Failed to exec cmd '%s': %s", strings.Join(cmd.Args, " "), string(output))
		return err
	}
	return nil
}

func GetSystemdResolveCmd(ctx context.Context, tunName string, config *miekgdns.ClientConfig) error {
	cmd := exec.CommandContext(ctx, "systemd-resolve", []string{
		"--set-dns",
		config.Servers[0],
		"--interface",
		tunName,
		"--set-domain=" + config.Search[0],
		"--set-domain=" + config.Search[1],
		"--set-domain=" + config.Search[2],
	}...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Debugf("Failed to exec cmd '%s': %s", strings.Join(cmd.Args, " "), string(output))
	}
	return err
}

var ErrorNotSupportSplitDNS = errors.New("not support split DNS")

func (c *Config) UseLibraryDNS(tunName string, clientConfig *miekgdns.ClientConfig) error {
	configurator, err := dns.NewOSConfigurator(log.Debugf, nil, nil, tunName)
	if err != nil {
		return err
	}
	if !configurator.SupportsSplitDNS() {
		return ErrorNotSupportSplitDNS
	}
	c.OSConfigurator = configurator
	config := dns.OSConfig{Nameservers: []netip.Addr{}, SearchDomains: []dnsname.FQDN{}}
	for _, s := range clientConfig.Servers {
		ip, ok := netip.AddrFromSlice(net.ParseIP(s))
		if !ok {
			continue
		}
		config.Nameservers = append(config.Nameservers, ip)
	}
	for _, search := range clientConfig.Search {
		fqdn, err := dnsname.ToFQDN(search)
		if err != nil {
			continue
		}
		config.SearchDomains = append(config.SearchDomains, fqdn)
	}
	return c.OSConfigurator.SetDNS(config)
}

func SetupLocalDNS(ctx context.Context, clientConfig *miekgdns.ClientConfig, existNameservers []string) error {
	corefile, err := BuildCoreFile(CoreFileTmpl{
		UpstreamDNS: clientConfig.Servers[0],
		Nameservers: strings.Join(existNameservers, " "),
	})
	if err != nil {
		return err
	}

	log.Debugf("Corefile content: %s", string(corefile.Body()))

	// Start your engines
	instance, err := caddy.Start(corefile)
	if err != nil {
		return err
	}

	// Twiddle your thumbs
	go instance.Wait()
	go func() {
		<-ctx.Done()
		instance.Stop()
	}()
	return nil
}

func (c *Config) CancelDNS() {
	c.removeHosts(sets.New[Entry]().Insert(c.Hosts...).UnsortedList())
	if c.OSConfigurator != nil && c.OSConfigurator.SupportsSplitDNS() {
		_ = c.OSConfigurator.Close()
	}

	filename := resolvconf.Path()
	readFile, err := os.ReadFile(filename)
	if err != nil {
		return
	}
	resolvConf, err := miekgdns.ClientConfigFromReader(bytes.NewBufferString(string(readFile)))
	if err != nil {
		return
	}
	for i := 0; i < len(resolvConf.Servers); i++ {
		if resolvConf.Servers[i] == c.Config.Servers[0] {
			resolvConf.Servers = append(resolvConf.Servers[:i], resolvConf.Servers[i+1:]...)
			i--
			break
		}
	}
	err = WriteResolvConf(resolvconf.Path(), *resolvConf)
	if err != nil {
		log.Warnf("Failed to remove DNS from resolv conf file: %v", err)
	}
}

func GetHostFile() string {
	return "/etc/hosts"
}

func WriteResolvConf(filename string, config miekgdns.ClientConfig) error {
	var options []string
	if config.Ndots != 0 {
		options = append(options, fmt.Sprintf("ndots:%d", config.Ndots))
	}
	if config.Attempts != 0 {
		options = append(options, fmt.Sprintf("attempts:%d", config.Attempts))
	}
	if config.Timeout != 0 {
		options = append(options, fmt.Sprintf("timeout:%d", config.Timeout))
	}

	_, err := resolvconf.Build(filename, config.Servers, config.Search, options)
	return err
}
