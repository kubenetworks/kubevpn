//go:build linux
// +build linux

package dns

import (
	"os"
	"os/exec"

	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

// systemd-resolve --status, systemd-resolve --flush-caches
func SetupDNS(config *miekgdns.ClientConfig, _ []string) error {
	tunName := os.Getenv("tunName")
	if len(tunName) == 0 {
		tunName = "tun0"
	}
	cmd := exec.Command("systemd-resolve", []string{
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
		log.Warnf("cmd: %s, output: %s, error: %v\n", cmd.Args, string(output), err)
	}

	return nil
}

func CancelDNS() {
}
