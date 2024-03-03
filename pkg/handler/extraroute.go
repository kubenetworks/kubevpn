package handler

import "github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"

type ExtraRouteInfo struct {
	ExtraCIDR   []string
	ExtraDomain []string
	ExtraNodeIP bool
}

func ParseExtraRouteFromRPC(route *rpc.ExtraRoute) *ExtraRouteInfo {
	if route == nil {
		return &ExtraRouteInfo{}
	}
	return &ExtraRouteInfo{
		ExtraCIDR:   route.ExtraCIDR,
		ExtraDomain: route.ExtraDomain,
		ExtraNodeIP: route.ExtraNodeIP,
	}
}

func (e ExtraRouteInfo) ToRPC() *rpc.ExtraRoute {
	return &rpc.ExtraRoute{
		ExtraCIDR:   e.ExtraCIDR,
		ExtraDomain: e.ExtraDomain,
		ExtraNodeIP: e.ExtraNodeIP,
	}
}
