package tun

import "github.com/containernetworking/cni/pkg/types"

// AddRoutes adds the given routes to the named TUN device's routing table.
func AddRoutes(tunName string, routes ...types.Route) error {
	return addTunRoutes(tunName, routes...)
}

// DeleteRoutes removes the given routes from the named TUN device's routing table.
func DeleteRoutes(tunName string, routes ...types.Route) error {
	return deleteTunRoutes(tunName, routes...)
}

// ListRoutes returns the routes kubevpn installed on the named TUN device, so callers can
// re-assert them. Only macOS needs this: its routes memorise a source address that goes stale
// when the TUN IP changes (see docs/52-stale-route-src-after-ip-change.md), while Linux and
// Windows routes are device-scoped and pick the source address at transmit time. On those
// platforms it returns nothing, making any refresh loop a no-op rather than a special case.
func ListRoutes(tunName string) ([]types.Route, error) {
	return listTunRoutes(tunName)
}
