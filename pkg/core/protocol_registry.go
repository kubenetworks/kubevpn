package core

import (
	"net"
	"strings"

	"github.com/containernetworking/cni/pkg/types"

	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

func init() {
	RegisterProtocol("tun", tunProtocolFactory)
	RegisterProtocol("gtcp", gtcpProtocolFactory)
	RegisterProtocol("gudp", gudpProtocolFactory)
	RegisterProtocol("ssh", sshProtocolFactory)
}

func tunProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	var forwarder *Forwarder
	if node.Forward != "" {
		var err error
		forwarder, err = ParseForwarder(node.Forward)
		if err != nil {
			return nil, nil, err
		}
	}
	handler := TunHandler(forwarder, hub)
	listener, err := tun.Listener(tun.Config{
		Name:    node.Get("name"),
		Addr:    node.Get("net"),
		Addr6:   node.Get("net6"),
		MTU:     node.GetInt("mtu"),
		Routes:  parseRoutes(node.Get("route")),
		Gateway: node.Get("gw"),
	})
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func parseRoutes(str string) []types.Route {
	var routes []types.Route
	for _, s := range strings.Split(str, ",") {
		if _, ipNet, _ := net.ParseCIDR(strings.TrimSpace(s)); ipNet != nil {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}
	return routes
}

func gtcpProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	handler := GvisorTCPHandler(hub)
	listener, err := GvisorTCPListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func gudpProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	handler := GvisorUDPHandler()
	listener, err := GvisorUDPListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}

func sshProtocolFactory(node *Node, hub *RouteHub) (net.Listener, Handler, error) {
	handler := SSHHandler()
	listener, err := SSHListener(node.Addr)
	if err != nil {
		return nil, nil, err
	}
	return listener, handler, nil
}
