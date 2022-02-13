//go:build windows
// +build windows

package pkg

import (
	"net"
)

func getRouteTable() (map[string][]*net.IPNet, error) {
	return make(map[string][]*net.IPNet), nil
}

func disableDevice(list []string) error {
	return nil
}
