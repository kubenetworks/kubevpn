package util

import (
	"bytes"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
	net2 "k8s.io/utils/net"
	"net"
	"os/exec"
	"strings"
)

// DetectAndDisableConflictDevice will detect conflict route table and try to disable device
// 1, get route table
// 2, detect conflict
// 3, disable device
func DetectAndDisableConflictDevice(origin string) error {
	routeTable, err := getRouteTableByNetstat()
	if err != nil {
		return err
	}
	conflict := detectConflictDevice(origin, routeTable)
	if len(conflict) != 0 {
		log.Infof("those device: %s will to be disabled because of route conflict with %s", strings.Join(conflict, ","), origin)
	}
	err = disableDevice(conflict)
	return err
}

// sudo ifconfig utun3 down
func disableDevice(conflict []string) error {
	for _, dev := range conflict {
		if err := exec.Command("sudo", "ifconfig", dev, "down").Run(); err != nil {
			log.Errorf("can not disable interface: %s, err: %v", dev, err)
			return err
		}
	}
	return nil
}

func detectConflictDevice(origin string, routeTable map[string][]*net.IPNet) []string {
	var conflict = sets.NewString()
	vpnRoute := routeTable[origin]
	for k, originRoute := range routeTable {
		if k == origin {
			continue
		}
	out:
		for _, originNet := range originRoute {
			for _, vpnNet := range vpnRoute {
				// should be same type
				if net2.IsIPv6(originNet.IP) && net2.IsIPv4(vpnNet.IP) ||
					net2.IsIPv4(originNet.IP) && net2.IsIPv6(vpnNet.IP) {
					continue
				}
				// like 255.255.0.0/16 should not take effect
				if bytes.Equal(originNet.IP, originNet.Mask) || bytes.Equal(vpnNet.IP, vpnNet.Mask) {
					continue
				}
				if intersect(vpnNet, originNet) {
					originMask, _ := originNet.Mask.Size()
					vpnMask, _ := vpnNet.Mask.Size()
					// means interface: k is more precisely, traffic will go to interface k because route table feature
					// mare precisely is preferred
					if originMask > vpnMask {
						conflict.Insert(k)
						break out
					}
				}
			}
		}
	}
	return conflict.Delete(origin).List()
}

func intersect(n1, n2 *net.IPNet) bool {
	for i := range n1.IP {
		if n1.IP[i]&n1.Mask[i] != n2.IP[i]&n2.Mask[i]&n1.Mask[i] {
			return false
		}
	}
	return true
}
