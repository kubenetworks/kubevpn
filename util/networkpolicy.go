package util

import (
	log "github.com/sirupsen/logrus"
	"os/exec"
	"time"
)

// DeleteWindowsFirewallRule Delete all action block firewall rule
func DeleteWindowsFirewallRule() {
	ticker := time.NewTicker(time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				_ = exec.Command("PowerShell", []string{
					"Remove-NetFirewallRule",
					"-Action",
					"Block",
				}...,
				).Start()
			}
		}
	}()
}

func AddFirewallRule() {
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"add",
		"rule",
		"name=" + TrafficManager,
		"dir=in",
		"action=allow",
		"enable=yes",
		"remoteip=223.254.254.1/24,LocalSubnet",
	}...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Infof("error while exec command: %s, out: %s, err: %v", cmd.Args, string(out), err)
	}
}

func FindRule() bool {
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"show",
		"rule",
		"name=" + TrafficManager,
	}...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Infof("find route out: %s error: %v", string(out), err)
		return false
	} else {
		return true
	}
}
