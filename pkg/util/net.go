package util

import (
	"fmt"
	"net"
	"strings"
)

func GetTunDevice(ips ...net.IP) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, i := range interfaces {
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			for _, ip := range ips {
				if strings.Contains(addr.String(), ip.String()) {
					return &i, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("can not found any interface with ip %v", ips)
}

func GetTunDeviceByConn(tun net.Conn) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var ip string
	if tunIP, ok := tun.LocalAddr().(*net.IPNet); ok {
		ip = tunIP.IP.String()
	} else {
		ip = tun.LocalAddr().String()
	}
	for _, i := range interfaces {
		addrs, err := i.Addrs()
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if strings.Contains(addr.String(), tun.LocalAddr().String()) {
				return &i, nil
			}
		}
	}
	return nil, fmt.Errorf("can not found any interface with ip %v", ip)
}
