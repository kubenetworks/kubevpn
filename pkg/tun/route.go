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
