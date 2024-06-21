//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package gateway

import (
	"net"
	"os/exec"
	"syscall"

	"golang.org/x/net/route"
)

func readNetstat() ([]byte, error) {
	routeCmd := exec.Command("netstat", "-rn")
	return routeCmd.CombinedOutput()
}

func discoverGatewayOSSpecific() (ip net.IP, err error) {
	rib, err := route.FetchRIB(syscall.AF_INET, syscall.NET_RT_DUMP, 0)
	if err != nil {
		return nil, err
	}

	msgs, err := route.ParseRIB(syscall.NET_RT_DUMP, rib)
	if err != nil {
		return nil, err
	}

	for _, m := range msgs {
		switch m := m.(type) {
		case *route.RouteMessage:
			var ip net.IP
			switch sa := m.Addrs[syscall.RTAX_GATEWAY].(type) {
			case *route.Inet4Addr:
				ip = net.IPv4(sa.IP[0], sa.IP[1], sa.IP[2], sa.IP[3])
				return ip, nil
			case *route.Inet6Addr:
				ip = make(net.IP, net.IPv6len)
				copy(ip, sa.IP[:])
				return ip, nil
			}
		}
	}
	return nil, &ErrNoGateway{}
}

func discoverGatewayInterfaceOSSpecific() (ip net.IP, err error) {
	bytes, err := readNetstat()
	if err != nil {
		return nil, err
	}

	return parseUnixInterfaceIP(bytes)
}
