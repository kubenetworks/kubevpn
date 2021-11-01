//go:build windows
// +build windows

package dns

import (
	"context"
	"fmt"
	miekgdns "github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
	"net"
	"os"
	"os/exec"
	"strconv"
)

func SetupDNS(config miekgdns.ClientConfig) error {
	getenv := os.Getenv("luid")
	parseUint, err := strconv.ParseUint(getenv, 10, 64)
	if err != nil {
		log.Warningln(err)
		return err
	}
	luid := winipcfg.LUID(parseUint)
	err = luid.SetDNS(windows.AF_INET, []net.IP{net.ParseIP(config.Servers[0])}, config.Search)
	_ = exec.CommandContext(context.Background(), "ipconfig", "/flushdns").Run()
	if err != nil {
		log.Warningln(err)
		return err
	}
	//_ = updateNicMetric(tunName)
	return nil
}

func CancelDNS() {
	getenv := os.Getenv("luid")
	parseUint, err := strconv.ParseUint(getenv, 10, 64)
	if err != nil {
		log.Warningln(err)
		return
	}
	luid := winipcfg.LUID(parseUint)
	_ = luid.FlushDNS(windows.AF_INET)
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
