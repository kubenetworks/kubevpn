//go:build !windows || ignore || !darwin
// +build !windows ignore !darwin

package pkg

import (
	"net"
	"os/exec"
	"strings"
)

func getRouteTable() (map[string][]*net.IPNet, error) {
	output, err := exec.Command("route", "-n").CombinedOutput()
	if err != nil {
		return nil, err
	}
	split := strings.Split(string(output), "\n")
	routeTable := make(map[string][]*net.IPNet)
	for _, i := range split {
		fields := strings.Fields(i)
		if len(fields) >= 8 {
			dst := net.ParseIP(fields[0])
			mask := make(net.IPMask, net.IPv4len)
			copy(mask, net.ParseIP(fields[2]))
			eth := fields[7]
			if v, ok := routeTable[eth]; ok {
				routeTable[eth] = append(v, &net.IPNet{IP: dst, Mask: mask})
			} else {
				routeTable[eth] = []*net.IPNet{{IP: dst, Mask: mask}}
			}
		}
	}
	return routeTable, nil
}

func disableDevice(list []string) error {
	for _, dev := range list {
		if err := exec.Command("sudo", "ifconfig", dev, "down").Run(); err != nil {
			return err
		} else {
			rollbackFuncList = append(rollbackFuncList, func() {
				_ = exec.Command("sudo", "ifconfig", dev, "up").Run()
			})
		}

	}
	return nil
}
