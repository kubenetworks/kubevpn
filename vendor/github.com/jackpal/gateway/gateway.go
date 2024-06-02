package gateway

import (
	"fmt"
	"net"
	"runtime"
)

// ErrNoGateway is returned if a valid gateway entry was not
// found in the route table.
type ErrNoGateway struct{}

// ErrCantParse is returned if the route table is garbage.
type ErrCantParse struct{}

// ErrNotImplemented is returned if your operating system
// is not supported by this package. Please raise an issue
// to request support.
type ErrNotImplemented struct{}

// ErrInvalidRouteFileFormat is returned if the format
// of /proc/net/route is unexpected on Linux systems.
// Please raise an issue.
type ErrInvalidRouteFileFormat struct {
	row string
}

func (*ErrNoGateway) Error() string {
	return "no gateway found"
}

func (*ErrCantParse) Error() string {
	return "can't parse route table"
}

func (*ErrNotImplemented) Error() string {
	return "not implemented for OS: " + runtime.GOOS
}

func (e *ErrInvalidRouteFileFormat) Error() string {
	return fmt.Sprintf("invalid row %q in route file: doesn't have 11 fields", e.row)
}

// DiscoverGateway is the OS independent function to get the default gateway
func DiscoverGateway() (ip net.IP, err error) {
	return discoverGatewayOSSpecific()
}

// DiscoverInterface is the OS independent function to call to get the default network interface IP that uses the default gateway
func DiscoverInterface() (ip net.IP, err error) {
	return discoverGatewayInterfaceOSSpecific()
}
