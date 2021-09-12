//go:build windows
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
	log.Info("tun name: " + tunName)
	_ = cleanDnsServer(tunName)
	cmd := exec.Command("netsh", []string{
		"interface",
		"ipv4",
		"add",
		"dnsservers",
		fmt.Sprintf("name=\"%s\"", tunName),
		fmt.Sprintf("address=%s", ip),
		"index=1",
	}...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Warnf("error while set dns server, error: %v, output: %s, command: %v", err, string(output), cmd.Args)
	}
	_ = addNicSuffixSearchList(namespace)
	_ = updateNicMetric(tunName)
	return nil
}

// @see https://docs.microsoft.com/en-us/powershell/module/dnsclient/set-dnsclientglobalsetting?view=windowsserver2019-ps#example-1--set-the-dns-suffix-search-list
func addNicSuffixSearchList(namespace string) error {
	cmd := exec.Command("PowerShell", []string{
		"Set-DnsClientGlobalSetting",
		"-SuffixSearchList",
		fmt.Sprintf("@(\"%s.svc.cluster.local\", \"svc.cluster.local\")", namespace),
	}...)
	output, err := cmd.CombinedOutput()
	log.Info(cmd.Args)
	if err != nil {
		log.Warnf("error while set dns suffix search list, err: %v, output: %s, command: %v", err, string(output), cmd.Args)
	}
	return err
}

func updateNicMetric(name string) error {
	cmd := exec.Command("PowerShell", []string{
		"Set-NetIPInterface",
		"-InterfaceAlias",
		fmt.Sprintf("\"%s\"", name),
		"-InterfaceMetric",
		"1",
	}...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warnf("error while update nic metrics, error: %v, output: %s, command: %v", err, string(out), cmd.Args)
	}
	return err
}

func cleanDnsServer(name string) error {
	cmd := exec.Command("netsh", []string{
		"interface",
		"ipv4",
		"delete",
		"dnsservers",
		fmt.Sprintf("\"%s\"", name),
		"all",
	}...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warnf("clean dnsservers failed, error: %v, output: %s, command: %v", err, string(out), cmd.Args)
	}
	return err
}
