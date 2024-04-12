//go:build linux

package dns

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/coredns/caddy"
	_ "github.com/coredns/coredns/core/dnsserver"
	_ "github.com/coredns/coredns/core/plugin"
	"github.com/docker/docker/libnetwork/resolvconf"
	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

// systemd-resolve --status, systemd-resolve --flush-caches
func (c *Config) SetupDNS(ctx context.Context) error {
	clientConfig := c.Config
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

	cmd := exec.Command("systemd-resolve", []string{
		"--set-dns",
		clientConfig.Servers[0],
		"--interface",
		tunName,
		"--set-domain=" + clientConfig.Search[0],
		"--set-domain=" + clientConfig.Search[1],
		"--set-domain=" + clientConfig.Search[2],
	}...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Debugf("failed to exec cmd: %s, message: %s, ignore", strings.Join(cmd.Args, " "), string(output))
		err = nil
	}

	filename := resolvconf.Path()
	readFile, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	localResolvConf, err := miekgdns.ClientConfigFromReader(bytes.NewBufferString(string(readFile)))
	if err != nil {
		return err
	}
	localResolvConf.Servers = append([]string{clientConfig.Servers[0]}, localResolvConf.Servers...)
	err = WriteResolvConf(*localResolvConf)
	return err
}

func SetupLocalDNS(ctx context.Context, clientConfig *miekgdns.ClientConfig, existNameservers []string) error {
	corefile, err := BuildCoreFile(CoreFileTmpl{
		UpstreamDNS: clientConfig.Servers[0],
		Nameservers: strings.Join(existNameservers, " "),
	})
	if err != nil {
		return err
	}

	log.Debugf("corefile content: %s", string(corefile.Body()))

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
	c.removeHosts(c.Hosts)

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
	err = WriteResolvConf(*resolvConf)
	if err != nil {
		log.Warnf("failed to remove dns from resolv conf file: %v", err)
	}
}

func GetHostFile() string {
	return "/etc/hosts"
}

func WriteResolvConf(config miekgdns.ClientConfig) error {
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

	filename := filepath.Join("/", "etc", "resolv.conf")
	_, err := resolvconf.Build(filename, config.Servers, config.Search, options)
	return err
}
