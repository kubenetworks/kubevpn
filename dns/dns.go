package dns

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"os"
	"os/exec"
)

func Windows(ip string) error {
	tunName := os.Getenv("tunName")
	fmt.Println("tun name: " + tunName)
	args := []string{
		"interface",
		"ipv4",
		"add",
		"dnsserver",
		fmt.Sprintf("name=\"%s\"", tunName),
		fmt.Sprintf("address=%s", ip),
		"index=2",
	}
	output, err := exec.Command("netsh", args...).CombinedOutput()
	log.Info(string(output))
	if err != nil {
		log.Error(err)
		return nil
	}
	return nil
}
