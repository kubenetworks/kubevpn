//go:build windows

package dns

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// SetupDNS configures cluster DNS servers on the TUN interface using Windows LUID APIs.
//
// It is best-effort, matching the Linux behaviour (dns_linux.go): programming the resolver
// can fail with "Access is denied" even in an elevated process when security software, an
// EDR agent, or group policy locks DNS configuration (the underlying iphlpapi
// SetInterfaceDnsSettings call is intercepted). Such a failure must NOT abort the whole
// connection — the TUN device and routes are already up, and cluster Service names still
// resolve via the hosts file entries pushed by the traffic manager. So on total failure we
// log an actionable warning and return nil; other cluster FQDNs (e.g. raw pod DNS) may not
// resolve on such hosts, which is the same trade-off Linux makes without a split DNS manager.
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
			plog.G(ctx).Errorf("Parse %s failed: %v", s, err)
			return err
		}
		servers = append(servers, addr.Unmap())
	}

	// Program each family independently: a machine with IPv6 disabled must not let the
	// AF_INET6 attempt abort a successful AF_INET setup (and vice versa). We only judge the
	// families that actually have a nameserver to set — clearing an empty family is a no-op
	// whose failure should not trigger the degraded-mode warning.
	var attemptedAny, okAny bool
	for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
		attempted, ok := c.setDNSForFamily(ctx, luid, tun.Index, family, servers, clientConfig.Search)
		attemptedAny = attemptedAny || attempted
		if attempted && ok {
			okAny = true
		}
	}

	if attemptedAny && !okAny {
		plog.G(ctx).Warnf("Could not set cluster DNS on TUN interface %q (access denied). This is "+
			"usually caused by security software, an EDR agent, or group policy that locks DNS "+
			"settings. Cluster Service names still resolve via the hosts file; other cluster FQDNs "+
			"(e.g. raw pod DNS) may not. Re-run with --debug to see the underlying error.", tunName)
	}
	return nil
}

// setDNSForFamily sets the cluster DNS servers for a single address family on the TUN
// interface. It first tries the winipcfg LUID API (SetInterfaceDnsSettings); on any error it
// falls back to netsh. It reports whether the family was actually attempted (had at least one
// matching nameserver) and whether it ultimately succeeded.
func (c *Config) setDNSForFamily(ctx context.Context, luid winipcfg.LUID, ifIndex int, family winipcfg.AddressFamily, servers []netip.Addr, search []string) (attempted bool, ok bool) {
	familyServers := filterServersForFamily(family, servers)
	attempted = len(familyServers) > 0

	// winipcfg.SetDNS filters `servers` to this family internally, so pass the full list.
	err := luid.SetDNS(family, servers, search)
	if err == nil {
		return attempted, true
	}
	if !attempted {
		// Nothing meaningful to set for this family (e.g. no IPv6 nameservers). Clearing it
		// failed, but that is irrelevant to whether cluster DNS is usable.
		plog.G(ctx).Debugf("Clear DNS (family=%s) on %q failed (no servers, ignored): %v", familyName(family), c.TunName, err)
		return false, false
	}

	plog.G(ctx).Debugf("Set DNS (family=%s) on %q via API failed: %v; trying netsh fallback", familyName(family), c.TunName, err)
	if nerr := setDNSByNetsh(ctx, ifIndex, family, familyServers); nerr != nil {
		plog.G(ctx).Debugf("Set DNS (family=%s) on %q via netsh failed: %v", familyName(family), c.TunName, nerr)
		return true, false
	}
	return true, true
}

// applyResolvers is a no-op on Windows: per-service resolver files are macOS-only.
func (c *Config) applyResolvers(_ context.Context) {}

// CancelDNS flushes DNS and route entries from the TUN interface and removes managed hosts entries.
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

// filterServersForFamily returns the subset of servers belonging to the given address family.
func filterServersForFamily(family winipcfg.AddressFamily, servers []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, s := range servers {
		if (s.Is4() && family == windows.AF_INET) || (s.Is6() && family == windows.AF_INET6) {
			out = append(out, s)
		}
	}
	return out
}

func familyName(family winipcfg.AddressFamily) string {
	if family == windows.AF_INET6 {
		return "ipv6"
	}
	return "ipv4"
}

const (
	netshTmplFlush4 = "interface ipv4 set dnsservers name=%d source=static address=none validate=no register=both"
	netshTmplFlush6 = "interface ipv6 set dnsservers name=%d source=static address=none validate=no register=both"
	netshTmplAdd4   = "interface ipv4 add dnsservers name=%d address=%s validate=no"
	netshTmplAdd6   = "interface ipv6 add dnsservers name=%d address=%s validate=no"
)

// buildNetshDNSCmds builds the netsh command sequence that programs the given DNS servers on
// the interface identified by ifIndex for one address family. It returns nil when there is
// nothing to set (unknown family or no matching servers).
func buildNetshDNSCmds(ifIndex int, family winipcfg.AddressFamily, servers []netip.Addr) []string {
	var flush, add string
	switch family {
	case windows.AF_INET:
		flush, add = netshTmplFlush4, netshTmplAdd4
	case windows.AF_INET6:
		flush, add = netshTmplFlush6, netshTmplAdd6
	default:
		return nil
	}
	familyServers := filterServersForFamily(family, servers)
	if len(familyServers) == 0 {
		return nil
	}
	cmds := make([]string, 0, 1+len(familyServers))
	cmds = append(cmds, fmt.Sprintf(flush, ifIndex))
	for _, s := range familyServers {
		cmds = append(cmds, fmt.Sprintf(add, ifIndex, s.String()))
	}
	return cmds
}

// setDNSByNetsh programs DNS servers for one family via the netsh CLI as a fallback for the
// winipcfg LUID API. Mirrors winipcfg's own netsh fallback, which the vendored SetDNS only
// invokes on ERROR_PROC_NOT_FOUND (Windows < 1809) — never on ERROR_ACCESS_DENIED.
func setDNSByNetsh(ctx context.Context, ifIndex int, family winipcfg.AddressFamily, servers []netip.Addr) error {
	cmds := buildNetshDNSCmds(ifIndex, family, servers)
	if len(cmds) == 0 {
		return nil
	}
	return runNetshDNS(ctx, cmds)
}

func runNetshDNS(_ context.Context, cmds []string) error {
	system32, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(system32, "netsh.exe"))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open netsh stdin pipe: %w", err)
	}
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, strings.Join(append(cmds, "exit\r\n"), "\r\n"))
	}()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh: %w: %q", err, strings.TrimSpace(string(output)))
	}
	return nil
}
