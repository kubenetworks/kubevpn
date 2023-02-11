//go:build linux
// +build linux

package dns

import (
	"os"
	"os/exec"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// systemd-resolve --status, systemd-resolve --flush-caches
func SetupDNS(clientConfig *miekgdns.ClientConfig, _ []string) error {
	tunName := os.Getenv(config.EnvTunNameOrLUID)
	if len(tunName) == 0 {
		tunName = "tun0"
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
		log.Warnf("cmd: %s, output: %s, error: %v\n", cmd.Args, string(output), err)
	}

	return nil
}

func CancelDNS() {
	updateHosts("")
}

func GetHostFile() string {
	return "/etc/hosts"
}
