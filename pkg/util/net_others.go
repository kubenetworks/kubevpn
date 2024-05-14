//go:build !windows

package util

import "net"

func IsIPv6Enabled() (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, err
	}

	ipv6Enabled := false
	for _, addr := range addrs {
		// Type assertion to net.IPNet to get the IP address without the mask.
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To16() != nil {
			if ipNet.IP.To4() == nil { // This is an IPv6 address
				ipv6Enabled = true
				break
			}
		}
	}

	return ipv6Enabled, nil
}
