package util

import (
	"context"
	"os/exec"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// DeleteWindowsFirewallRule Delete all action block firewall rule
func DeleteWindowsFirewallRule(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.Tick(time.Second * 10):
			_ = exec.Command("PowerShell", []string{
				"Remove-NetFirewallRule",
				"-Action",
				"Block",
			}...).Run()
		}
	}
}

func AddFirewallRule() {
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"add",
		"rule",
		"name=" + config.PodTrafficManager,
		"dir=in",
		"action=allow",
		"enable=yes",
		"remoteip=" + config.CIDR.String() + ",LocalSubnet",
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
		"name=" + config.PodTrafficManager,
	}...)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Infof("find route out: %s error: %v", string(out), err)
		return false
	} else {
		return true
	}
}
