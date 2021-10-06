//go:build linux
// +build linux

package dns

import (
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
)

// systemd-resolve --status, systemd-resolve --flush-caches
func SetupDNS(ip string, namespace string) error {
	tunName := os.Getenv("tunName")
	if len(tunName) == 0 {
		tunName = "tun0"
	}
	cmd := exec.Command("systemd-resolve", []string{
		"--set-dns",
		ip,
		"--interface",
		tunName,
		"--set-domain=" + namespace + ".svc.cluster.local",
		"--set-domain=svc.cluster.local",
	}...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Warnf("cmd: %s, output: %s, error: %v\n", cmd.Args, string(output), err)
	}

	return nil
}

func CancelDNS() {
}
