package util

import (
	"context"
	"os/exec"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/text/encoding/simplifiedchinese"

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
		"name=" + config.ConfigMapPodTrafficManager,
		"dir=in",
		"action=allow",
		"enable=yes",
		"remoteip=" + config.CIDR.String() + ",LocalSubnet",
	}...)
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

func DeleteFirewallRule() {
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"delete",
		"rule",
		"name=" + config.ConfigMapPodTrafficManager,
	}...)
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

func FindRule() bool {
	cmd := exec.Command("netsh", []string{
		"advfirewall",
		"firewall",
		"show",
		"rule",
		"name=" + config.ConfigMapPodTrafficManager,
	}...)
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
