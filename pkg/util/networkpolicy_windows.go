//go:build windows

package util

import (
	"context"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

/**
When startup an app listen 0.0.0.0 on Windows

Windows Security Alert
[x] Private networks,such as my home or work network
[ ] Public networks, such as those in airports and coffee shops (not recommended because these networks often have little or no security)

if not select the second options, Windows add a firewall rule like:

Get-NetFirewallRule -Direction Inbound -Action Block | Sort-Object -Property Priority

Name                          : {9127CE75-0943-4877-B797-1316948CDCA8}
DisplayName                   : ___go_build_authors.exe
Description                   : ___go_build_authors.exe
DisplayGroup                  :
Group                         :
Enabled                       : True
Profile                       : Public
Platform                      : {}
Direction                     : Inbound
Action                        : Block
EdgeTraversalPolicy           : Block
LooseSourceMapping            : False
LocalOnlyMapping              : False
Owner                         :
PrimaryStatus                 : OK
Status                        : The rule was parsed successfully from the store. (65536)
EnforcementStatus             : NotApplicable
PolicyStoreSource             : PersistentStore
PolicyStoreSourceType         : Local
RemoteDynamicKeywordAddresses :
PolicyAppId                   :

this makes tunIP can not access local service, so we need to delete this rule
*/
// DeleteBlockFirewallRule Delete all action block firewall rule
func DeleteBlockFirewallRule(ctx context.Context) {
	var deleteFirewallBlockRule = func() {
		// PowerShell Remove-NetFirewallRule -Action Block
		cmd := exec.CommandContext(ctx, "PowerShell", []string{"Remove-NetFirewallRule", "-Action", "Block"}...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_, _ = cmd.CombinedOutput()
		/*if err != nil && out != nil {
			s := string(out)
			var b []byte
			if b, err = decode(out); err == nil {
				s = string(b)
			}
			log.Debugf("failed to delete firewall rule: %v", s)
		}*/
	}

	deleteFirewallBlockRule()

	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleteFirewallBlockRule()
		}
	}
}

func decode(in []byte) ([]byte, error) {
	out, err := simplifiedchinese.GB18030.NewDecoder().Bytes(in)
	if err == nil {
		return out, err
	}
	out, err = simplifiedchinese.GBK.NewDecoder().Bytes(in)
	if err == nil {
		return out, err
	}
	return nil, err
}
