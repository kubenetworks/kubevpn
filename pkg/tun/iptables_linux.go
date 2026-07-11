//go:build linux

package tun

import (
	"net"
	"os/exec"
	"strings"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// UpdateDNAT replaces the PREROUTING DNAT target IP in iptables.
// Only effective in VPN-only mode (where DNAT targets an IP address).
// In mesh mode (DNAT to port :15006), this is a no-op.
func UpdateDNAT(oldIP, newIP net.IP) error {
	if oldIP == nil || newIP == nil {
		return nil
	}
	output, err := exec.Command("iptables", "-t", "nat", "-S", "PREROUTING").Output()
	if err != nil {
		return nil
	}
	if !strings.Contains(string(output), oldIP.String()) {
		return nil
	}

	err = exec.Command("iptables", "-t", "nat", "-R", "PREROUTING", "1",
		"!", "-p", "icmp", "-j", "DNAT", "--to", newIP.String()).Run()
	if err != nil {
		plog.G(nil).Warnf("Failed to update iptables IPv4 DNAT: %v", err)
	}

	if newIP.To4() == nil {
		_ = exec.Command("ip6tables", "-t", "nat", "-R", "PREROUTING", "1",
			"!", "-p", "icmp", "-j", "DNAT", "--to", newIP.String()).Run()
	}
	return err
}
