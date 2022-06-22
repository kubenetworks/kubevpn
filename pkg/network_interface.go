package pkg

import (
	"bytes"
	"net"
	"strings"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"
)

// DetectAndDisableConflictDevice will detect conflict route table and try to disable device
// 1, get route table
// 2, detect conflict
// 3, disable device
func DetectAndDisableConflictDevice(origin string) error {
	routeTable, err := getRouteTable()
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
				// like 255.255.0.0/16 should not take effect
				if bytes.Equal(originNet.IP, originNet.Mask) || bytes.Equal(vpnNet.IP, vpnNet.Mask) {
					continue
				}
				if vpnNet.Contains(originNet.IP) || originNet.Contains(vpnNet.IP) {
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
