//go:build windows

package util

import (
	"context"
	"os/exec"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// DeleteBlockFirewallRule Delete all action block firewall rule
func DeleteBlockFirewallRule(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.Tick(time.Second * 10):
			// PowerShell Remove-NetFirewallRule -Action Block
			cmd := exec.Command("PowerShell", []string{
				"Remove-NetFirewallRule",
				"-Action",
				"Block",
			}...)
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			cmd.Run()
		}
	}
}

func AddAllowFirewallRule() {
	// netsh advfirewall firewall add rule name=kubevpn-traffic-manager dir=in action=allow enable=yes remoteip=223.254.0.100/16,LocalSubnet
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"add",
		"rule",
		"name=" + config.ConfigMapPodTrafficManager,
		"dir=in",
		"action=allow",
		"enable=yes",
		"remoteip=" + config.CIDR.String() + ",LocalSubnet",
	}...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		var s string
		var b []byte
		if b, err = decode(out); err == nil {
			s = string(b)
		} else {
			s = string(out)
		}
		log.Infof("error while exec command: %s, out: %s", cmd.Args, s)
	}
}

func DeleteAllowFirewallRule() {
	// netsh advfirewall firewall delete rule name=kubevpn-traffic-manager
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"delete",
		"rule",
		"name=" + config.ConfigMapPodTrafficManager,
	}...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		var s string
		var b []byte
		if b, err = decode(out); err == nil {
			s = string(b)
		} else {
			s = string(out)
		}
		log.Errorf("error while exec command: %s, out: %s", cmd.Args, s)
	}
}

func FindAllowFirewallRule() bool {
	// netsh advfirewall firewall show rule name=kubevpn-traffic-manager
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"show",
		"rule",
		"name=" + config.ConfigMapPodTrafficManager,
	}...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		s := string(out)
		var b []byte
		if b, err = decode(out); err == nil {
			s = string(b)
		}
		log.Debugf("find route out: %s", s)
		return false
	} else {
		return true
	}
}

func decode(in []byte) (out []byte, err error) {
	out = in
	out, err = simplifiedchinese.GB18030.NewDecoder().Bytes(in)
	if err == nil {
		return
	}
	out, err = simplifiedchinese.GBK.NewDecoder().Bytes(in)
	if err == nil {
		return
	}
	return
}
