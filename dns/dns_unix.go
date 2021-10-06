//go:build darwin
// +build darwin

package dns

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"os/exec"
	"strings"
)

func SetupDNS(ip string, namespace string) error {
	/*var err error
	_ = os.RemoveAll(filepath.Join("/", "etc", "resolver"))
	if err = os.MkdirAll(filepath.Join("/", "etc", "resolver"), fs.ModePerm); err != nil {
		log.Error(err)
	}
	filename := filepath.Join("/", "etc", "resolver", "local")
	fileContent := "nameserver " + ip
	_ = ioutil.WriteFile(filename, []byte(fileContent), fs.ModePerm)

	filename = filepath.Join("/", "etc", "resolver", namespace)
	fileContent = "nameserver " + ip + "\nsearch " + namespace + ".svc.cluster.local svc.cluster.local cluster.local\noptions ndots:5"
	_ = ioutil.WriteFile(filename, []byte(fileContent), fs.ModePerm)*/
	networkSetup(ip, namespace)
	return nil
}

func CancelDNS() {
	networkCancel()
}

/*
➜  resolver sudo networksetup -setdnsservers Wi-Fi 172.20.135.131 1.1.1.1
➜  resolver sudo networksetup -setsearchdomains Wi-Fi test.svc.cluster.local svc.cluster.local cluster.local
➜  resolver sudo networksetup -getsearchdomains Wi-Fi
test.svc.cluster.local
svc.cluster.local
cluster.local
➜  resolver sudo networksetup -getdnsservers Wi-Fi
172.20.135.131
1.1.1.1
*/
func networkSetup(ip string, namespace string) {
	networkCancel()
	b, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return
	}
	services := strings.Split(string(b), "\n")
	for _, s := range services[:len(services)-1] {
		cmd := exec.Command("networksetup", "-getdnsservers", s)
		output, err := cmd.Output()
		if err == nil {
			var nameservers []string
			if strings.Contains(string(output), "There aren't any") {
				nameservers = make([]string, 0, 0)
			} else {
				nameservers = strings.Split(string(output), "\n")
				nameservers = nameservers[:len(nameservers)-1]
			}
			newNameservers := make([]string, len(nameservers)+1, len(nameservers)+1)
			copy(newNameservers[1:], nameservers)
			newNameservers[0] = ip
			args := []string{"-setdnsservers", s}
			output, err = exec.Command("networksetup", append(args, newNameservers...)...).Output()
			if err != nil {
				log.Warnf("error while set dnsserver for %s, err: %v, output: %s\n", s, err, string(output))
			}
		}
		output, err = exec.Command("networksetup", "-getsearchdomains", s).Output()
		if err == nil {
			var searchDomains []string
			if strings.Contains(string(output), "There aren't any Search Domains") {
				searchDomains = make([]string, 0, 0)
			} else {
				searchDomains = strings.Split(string(output), "\n")
				searchDomains = searchDomains[:len(searchDomains)-1]
			}
			newSearchDomains := make([]string, len(searchDomains)+3, len(searchDomains)+3)
			copy(newSearchDomains[3:], searchDomains)
			newSearchDomains[0] = fmt.Sprintf("%s.svc.cluster.local", namespace)
			newSearchDomains[1] = "svc.cluster.local"
			newSearchDomains[2] = "cluster.local"
			args := []string{"-setsearchdomains", s}
			bytes, err := exec.Command("networksetup", append(args, newSearchDomains...)...).Output()
			if err != nil {
				log.Warnf("error while set search domain for %s, err: %v, output: %s\n", s, err, string(bytes))
			}
		}
	}
}

func networkCancel() {
	b, err := exec.Command("networksetup", "-listallnetworkservices").CombinedOutput()
	if err != nil {
		return
	}
	services := strings.Split(string(b), "\n")
	for _, s := range services[:len(services)-1] {
		output, err := exec.Command("networksetup", "-getsearchdomains", s).Output()
		if err == nil {
			i := strings.Split(string(output), "\n")
			if i[1] == "svc.cluster.local" && i[2] == "cluster.local" {
				bytes, err := exec.Command("networksetup", "-setsearchdomains", s, strings.Join(i[3:], " ")).Output()
				if err != nil {
					log.Warnf("error while remove search domain for %s, err: %v, output: %s\n", s, err, string(bytes))
				}

				output, err := exec.Command("networksetup", "-getdnsservers", s).Output()
				if err == nil {
					dnsServers := strings.Split(string(output), "\n")
					dnsServers = dnsServers[1 : len(dnsServers)-1]
					if len(dnsServers) == 0 {
						// set default dns server to 1.1.1.1
						dnsServers = append(dnsServers, "1.1.1.1")
					}
					args := []string{"-setdnsservers", s}
					combinedOutput, err := exec.Command("networksetup", append(args, dnsServers...)...).Output()
					if err != nil {
						log.Warnf("error while remove dnsserver for %s, err: %v, output: %s\n", s, err, string(combinedOutput))
					}
				}
			}
		}
	}
}
