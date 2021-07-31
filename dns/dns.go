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
		"dnsservers",
		fmt.Sprintf("name=\"%s\"", tunName),
		fmt.Sprintf("address=%s", ip),
		"index=1",
	}
	output, err := exec.Command("netsh", args...).CombinedOutput()
	fmt.Println(exec.Command("netsh", args...).Args)
	log.Info(string(output))
	if err != nil {
		log.Error(err)
		return nil
	}
	return nil
}
