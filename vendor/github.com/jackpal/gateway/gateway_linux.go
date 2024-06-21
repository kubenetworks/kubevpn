//go:build linux
// +build linux

package gateway

import (
	"fmt"
	"io"
	"net"
	"os"
)

const (
	// See http://man7.org/linux/man-pages/man8/route.8.html
	file = "/proc/net/route"
)

func readRoutes() ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("can't access %s", file)
	}
	defer f.Close()

	bytes, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("can't read %s", file)
	}

	return bytes, nil
}

func discoverGatewayOSSpecific() (ip net.IP, err error) {
	bytes, err := readRoutes()
	if err != nil {
		return nil, err
	}
	return parseLinuxGatewayIP(bytes)
}

func discoverGatewayInterfaceOSSpecific() (ip net.IP, err error) {
	bytes, err := readRoutes()
	if err != nil {
		return nil, err
	}
	return parseLinuxInterfaceIP(bytes)
}
