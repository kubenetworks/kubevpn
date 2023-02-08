package handler

import (
	"crypto/tls"
	"net"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/core"
	"github.com/wencaiwulue/kubevpn/pkg/tun"
)

// Route example:
// -L "tcp://:10800" -L "tun://:8422?net=223.254.0.100/16"
// -L "tun:/10.233.24.133:8422?net=223.254.0.102/16&route=223.254.0.0/16"
// -L "tun:/127.0.0.1:8422?net=223.254.0.102/16&route=223.254.0.0/16,10.233.0.0/16" -F "tcp://127.0.0.1:10800"
type Route struct {
	ServeNodes []string // -L tun
	ChainNode  string   // -F tcp
	Retries    int
}

func (r *Route) parseChain() (*core.Chain, error) {
	// parse the base nodes
	node, err := parseChainNode(r.ChainNode)
	if err != nil {
		return nil, err
	}
	return core.NewChain(r.Retries, node), nil
}

func parseChainNode(ns string) (*core.Node, error) {
	node, err := core.ParseNode(ns)
	if err != nil {
		return nil, err
	}
	node.Client = &core.Client{
		Connector:   core.UDPOverTCPTunnelConnector(),
		Transporter: core.TCPTransporter(),
	}
	return node, nil
}

func (r *Route) GenerateServers() ([]core.Server, error) {
	chain, err := r.parseChain()
	if err != nil && !errors.Is(err, core.ErrorInvalidNode) {
		return nil, err
	}

	servers := make([]core.Server, 0, len(r.ServeNodes))
	for _, serveNode := range r.ServeNodes {
		node, err := core.ParseNode(serveNode)
		if err != nil {
			return nil, err
		}

		var ln net.Listener
		var handler core.Handler

		switch node.Protocol {
		case "tun":
			handler = core.TunHandler(chain, node)
			ln, err = tun.Listener(tun.Config{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				MTU:     node.GetInt("mtu"),
				Routes:  parseIPRoutes(node.Get("route")),
				Gateway: node.Get("gw"),
			})
			if err != nil {
				return nil, err
			}
		default:
			handler = core.TCPHandler()
			tcpListener, _ := core.TCPListener(node.Addr)
			ln = tls.NewListener(tcpListener, config.TlsConfigServer)
		}
		servers = append(servers, core.Server{Listener: ln, Handler: handler})
	}
	return servers, nil
}

func parseIPRoutes(routeStringList string) (routes []types.Route) {
	if len(routeStringList) == 0 {
		return
	}

	routeList := strings.Split(routeStringList, ",")
	for _, route := range routeList {
		if _, ipNet, _ := net.ParseCIDR(strings.TrimSpace(route)); ipNet != nil {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}
	return
}
