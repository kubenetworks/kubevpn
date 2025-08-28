package tun

import "github.com/containernetworking/cni/pkg/types"

// AddRoutes for outer called
func AddRoutes(tunName string, routes ...types.Route) error {
	return addTunRoutes(tunName, routes...)
}

// DeleteRoutes ...
func DeleteRoutes(tunName string, routes ...types.Route) error {
	return deleteTunRoutes(tunName, routes...)
}
