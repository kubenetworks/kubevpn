//go:build windows
// +build windows

package route

import (
	"net"
)

func GetRouteTable() (map[string][]*net.IPNet, error) {
	return make(map[string][]*net.IPNet), nil
}

func DisableDevice(list []string) error {
	return nil
}
