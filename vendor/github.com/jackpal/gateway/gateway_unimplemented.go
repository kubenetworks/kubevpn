//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows
// +build !darwin,!dragonfly,!freebsd,!linux,!netbsd,!openbsd,!solaris,!windows

package gateway

import (
	"net"
)

func discoverGatewayOSSpecific() (ip net.IP, err error) {
	return ip, &ErrNotImplemented{}
}

func discoverGatewayInterfaceOSSpecific() (ip net.IP, err error) {
	return nil, &ErrNotImplemented{}
}
