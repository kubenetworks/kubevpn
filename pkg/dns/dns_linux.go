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
}
