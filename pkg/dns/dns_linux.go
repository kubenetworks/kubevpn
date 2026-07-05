//go:build linux

package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"

	miekgdns "github.com/miekg/dns"
	"tailscale.com/net/dns"
	"tailscale.com/util/dnsname"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// SetupDNS configures the system DNS on Linux using systemd-resolved (resolvectl /
// systemd-resolve commands) or the tailscale library DNS configurator. In every case the
// cluster nameserver is scoped to cluster domains on the TUN interface (split DNS); it is
// never written to the global /etc/resolv.conf. Hosts without a split-capable DNS manager
// resolve cluster names via /etc/hosts only.
func (c *Config) SetupDNS(ctx context.Context) error {
	config := c.Config
	tunName := c.TunName

	// 1) use systemctl or resolvectl to setup dns
	plog.G(ctx).Debugf("Use systemd to setup DNS...")
	// TODO consider use https://wiki.debian.org/NetworkManager and nmcli to config DNS
	// try to solve:
	// sudo systemd-resolve --set-dns 172.28.64.10 --interface tun0 --set-domain=vke-system.svc.cluster.local --set-domain=svc.cluster.local --set-domain=cluster.local
	//Failed to set DNS configuration: Unit dbus-org.freedesktop.resolve1.service not found.
	// ref: https://superuser.com/questions/1427311/activation-via-systemd-failed-for-unit-dbus-org-freedesktop-resolve1-service
	// Best-effort start only. We deliberately do NOT `systemctl enable` it: enabling persistently
	// changes the user's boot configuration just to set up VPN DNS, which can disrupt machines that
	// use NetworkManager/dnsmasq instead. `systemctl status` is likewise dropped (its output was
	// discarded). If systemd-resolved isn't the DNS manager here, the resolvectl/systemd-resolve
	// attempts below simply fail and we fall through to the library path.
	_ = exec.Command("systemctl", "start", "systemd-resolved.service").Run()
	plog.G(ctx).Debugf("Started systemd-resolved (best-effort)...")
	var exists = func(cmd string) bool {
		_, err := exec.LookPath(cmd)
		return err == nil
	}
	plog.G(ctx).Debugf("Try to setup DNS by resolvectl or systemd-resolve...")
	if exists("resolvectl") {
		if setupDnsByCmdResolvectl(ctx, tunName, config) == nil {
			return nil
		}
	}
	if exists("systemd-resolve") {
		if setupDNSbyCmdSystemdResolve(ctx, tunName, config) == nil {
			return nil
		}
	}

	// 2) setup dns by the library (tailscale), scoped to cluster domains (split DNS)
	plog.G(ctx).Debugf("Use library to setup DNS...")
	err := c.UseLibraryDNS(ctx, tunName, config)
	if err == nil {
		plog.G(ctx).Debugf("Use library to setup DNS done")
		return nil
	}
	// No split-capable DNS manager (systemd-resolved / NetworkManager / (open)resolvconf) is
	// available. We deliberately do NOT fall back to writing the cluster nameserver into the
	// global /etc/resolv.conf, which would hijack all DNS on the host. Cluster service names
	// still resolve via /etc/hosts entries pushed by the traffic manager; other cluster FQDNs
	// (e.g. raw pod DNS) will not resolve on such hosts.
	if errors.Is(err, errNotSupportSplitDNS) {
		plog.G(ctx).Warnf("No split-capable DNS manager found; cluster DNS resolves via /etc/hosts only")
	} else {
		plog.G(ctx).Errorf("Setup DNS by library failed: %v", err)
	}
	return nil
}

// setupDnsByCmdResolvectl
// resolvectl dns utun0 10.10.129.161
// resolvectl domain utun0 default.svc.cluster.local svc.cluster.local cluster.local
func setupDnsByCmdResolvectl(ctx context.Context, tunName string, config *miekgdns.ClientConfig) error {
	if len(config.Servers) == 0 {
		return fmt.Errorf("no DNS server found in pod resolv.conf")
	}
	cmd := exec.CommandContext(ctx, "resolvectl", "dns", tunName, config.Servers[0])
	output, err := cmd.CombinedOutput()
	if err != nil {
		plog.G(ctx).Debugf("Failed to exec cmd '%s': %s", strings.Join(cmd.Args, " "), string(output))
		return err
	}
	cmd = exec.CommandContext(ctx, "resolvectl", append([]string{"domain", tunName}, config.Search...)...)
	output, err = cmd.CombinedOutput()
	if err != nil {
		plog.G(ctx).Debugf("Failed to exec cmd '%s': %s", strings.Join(cmd.Args, " "), string(output))
		return err
	}
	return nil
}

func setupDNSbyCmdSystemdResolve(ctx context.Context, tunName string, config *miekgdns.ClientConfig) error {
	if len(config.Servers) == 0 {
		return fmt.Errorf("no DNS server found in pod resolv.conf")
	}
	args := []string{"--set-dns", config.Servers[0], "--interface", tunName}
	for _, search := range config.Search {
		args = append(args, "--set-domain="+search)
	}
	cmd := exec.CommandContext(ctx, "systemd-resolve", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		plog.G(ctx).Debugf("Failed to exec cmd '%s': %s", strings.Join(cmd.Args, " "), string(output))
	}
	return err
}

var errNotSupportSplitDNS = errors.New("not support split DNS")

func (c *Config) UseLibraryDNS(ctx context.Context, tunName string, clientConfig *miekgdns.ClientConfig) error {
	configurator, err := dns.NewOSConfigurator(plog.G(ctx).Debugf, nil, nil, tunName)
	if err != nil {
		return err
	}
	if !configurator.SupportsSplitDNS() {
		return errNotSupportSplitDNS
	}
	c.OSConfigurator = configurator
	plog.G(ctx).Debugf("Setting up DNS...")
	return c.OSConfigurator.SetDNS(buildLibraryOSConfig(clientConfig))
}

// buildLibraryOSConfig builds the tailscale dns.OSConfig for split DNS. The cluster search
// domains are set as MatchDomains so ONLY those queries are routed to the cluster nameserver
// via the TUN interface; every other query keeps using the host's default resolver.
//
// A non-empty MatchDomains is precisely what makes the library configure a *split* resolver
// rather than hijacking the interface as the global default DNS route — see tailscale
// net/dns resolved.go: SetLinkDefaultRoute(_, len(config.MatchDomains) == 0).
func buildLibraryOSConfig(clientConfig *miekgdns.ClientConfig) dns.OSConfig {
	config := dns.OSConfig{}
	for _, s := range clientConfig.Servers {
		ip, ok := netip.AddrFromSlice(net.ParseIP(s))
		if !ok {
			continue
		}
		config.Nameservers = append(config.Nameservers, ip.Unmap())
	}
	for _, search := range clientConfig.Search {
		fqdn, err := dnsname.ToFQDN(search)
		if err != nil {
			continue
		}
		config.SearchDomains = append(config.SearchDomains, fqdn)
		config.MatchDomains = append(config.MatchDomains, fqdn)
	}
	return config
}

// applyResolvers is a no-op on Linux: DNS is configured once via the OS
// configurator; per-service resolver files are a macOS-only concept.
func (c *Config) applyResolvers(_ context.Context) {}

// CancelDNS reverts DNS changes made by SetupDNS and removes managed hosts entries.
//
// The library split config is reverted via OSConfigurator.Close(); resolvectl/systemd-resolve
// per-interface settings are dropped by systemd-resolved when the TUN device is destroyed.
// Nothing is written to the global /etc/resolv.conf, so there is nothing to undo there.
func (c *Config) CancelDNS() {
	_ = c.removeHosts()
	if c.OSConfigurator != nil {
		_ = c.OSConfigurator.Close()
	}
}

func getHostFile() string {
	return "/etc/hosts"
}
