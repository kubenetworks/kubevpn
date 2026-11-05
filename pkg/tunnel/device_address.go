package tunnel

import (
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetTunDeviceIPByName returns the IPs assigned to the named interface within kubevpn CIDRs.
func GetTunDeviceIPByName(tunName string) (ipv4, ipv6, dockerIPv4 net.IP, err error) {
	ifc, err := net.InterfaceByName(tunName)
	if err != nil {
		return
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return
	}
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok {
			if config.CIDR.Contains(ipNet.IP) {
				ipv4 = ipNet.IP
			}
			if config.CIDR6.Contains(ipNet.IP) {
				ipv6 = ipNet.IP
			}
		}
	}
	return
}
