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
	"k8s.io/apimachinery/pkg/util/sets"
)

// systemd-resolve --status, systemd-resolve --flush-caches
func (c *Config) SetupDNS(ctx context.Context) error {
	clientConfig := c.Config
	useLocalDNS := c.UseLocalDNS
	tunName := c.TunName

	existNameservers := make([]string, 0)
	existSearches := make([]string, 0)
	filename := filepath.Join("/", "etc", "resolv.conf")
	readFile, err := os.ReadFile(filename)
	if err == nil {
		resolvConf, err := miekgdns.ClientConfigFromReader(bytes.NewBufferString(string(readFile)))
		if err == nil {
			existNameservers = resolvConf.Servers
			existSearches = resolvConf.Search
		}
	}

	if useLocalDNS {
		if err = SetupLocalDNS(ctx, clientConfig, existNameservers); err != nil {
			return err
		}
		clientConfig.Servers[0] = "127.0.0.1"
	}

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

	// add only if not exists
	for _, nameserver := range existNameservers {
		if !sets.New[string](clientConfig.Servers...).Has(nameserver) {
			clientConfig.Servers = append(clientConfig.Servers, nameserver)
		}
	}
	for _, search := range existSearches {
		if !sets.New[string](clientConfig.Search...).Has(search) {
			clientConfig.Search = append(clientConfig.Search, search)
		}
	}

	if !c.Lite {
		err = os.Rename(filename, getBackupFilename(filename))
		if err != nil {
			log.Debugf("failed to backup resolv.conf: %s", err.Error())
		}
		err = WriteResolvConf(*clientConfig)
	}
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

	if !c.Lite {
		filename := filepath.Join("/", "etc", "resolv.conf")
		_ = os.Rename(getBackupFilename(filename), filename)
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

func getBackupFilename(filename string) string {
	return filename + ".kubevpn_backup"
}
