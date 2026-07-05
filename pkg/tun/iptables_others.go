//go:build !linux

package tun

import "net"

func UpdateDNAT(_, _ net.IP) error {
	return nil
}
