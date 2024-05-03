package handler

import (
	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

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

func AddExtraRoute(flags *pflag.FlagSet, route *ExtraRouteInfo) {
	flags.StringArrayVar(&route.ExtraCIDR, "extra-cidr", []string{}, "Extra cidr string, add those cidr network to route table, eg: --extra-cidr 192.168.0.159/24 --extra-cidr 192.168.1.160/32")
	flags.StringArrayVar(&route.ExtraDomain, "extra-domain", []string{}, "Extra domain string, the resolved ip will add to route table, eg: --extra-domain test.abc.com --extra-domain foo.test.com")
	flags.BoolVar(&route.ExtraNodeIP, "extra-node-ip", false, "Extra node ip, add cluster node ip to route table.")
}
