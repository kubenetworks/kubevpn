// +build windows

package dns

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
)

func DNS(ip string, namespace string) error {
	tunName := os.Getenv("tunName")
	fmt.Println("tun name: " + tunName)
	args := []string{
		"interface",
		"ipv4",
		"add",
		"dnsservers",
		fmt.Sprintf("name=\"%s\"", tunName),
		fmt.Sprintf("address=%s", ip),
		"index=1",
	}
	output, err := exec.Command("netsh", args...).CombinedOutput()
	fmt.Println(exec.Command("netsh", args...).Args)
	log.Info(string(output))
	_ = addNicSuffix(namespace)
	if err != nil {
		log.Error(err)
		return nil
	}
	return nil
}

// @see https://docs.microsoft.com/en-us/powershell/module/dnsclient/set-dnsclientglobalsetting?view=windowsserver2019-ps#example-1--set-the-dns-suffix-search-list
func addNicSuffix(namespace string) error {
	cmd := exec.Command("PowerShell", []string{
		"Set-DnsClientGlobalSetting",
		"-SuffixSearchList",
		fmt.Sprintf("@(\"%s.svc.cluster.local\", \"svc.cluster.local\")", namespace),
	}...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Warn(err)
	}
	log.Info(string(output))
	return err
}
