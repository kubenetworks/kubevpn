package dns

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"os/exec"
)

// todo set dns ip
// todo needs to test if set dns server do works or not
func Windows(clientset *kubernetes.Clientset) error {
	ip, err := GetDNSIp(clientset)
	if err != nil {
		return err
	}
	cmd := "interface ipv4 add dnsserver name=\"Ethernet\" address=%s  index=2"
	output, err := exec.Command("netsh", fmt.Sprintf(cmd, ip)).CombinedOutput()
	if err != nil {
		return err
	}
	log.Info(output)
	return nil
}

// todo update metrics, set default gateway
func UpdateMetric() error {
	// todo get tun name
	cmd := "interface ip set interface interface=\"Ethernet0\" metric=290"
	output, err := exec.Command("netsh", cmd).CombinedOutput()
	if err != nil {
		return err
	}
	log.Info(output)
	return nil
}
