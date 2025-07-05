package core

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

var (
	// RouteMapTCP map[srcIP]net.Conn Globe route table for inner ip
	RouteMapTCP = &sync.Map{}
	// TCPPacketChan tcp connects
	TCPPacketChan = make(chan *Packet, MaxSize)
)

// Route example:
// -l "tcp://:10800" -l "tun://:8422?net=198.19.0.100/16"
// -l "tun:/10.233.24.133:8422?net=198.19.0.102/16&route=198.19.0.0/16"
// -l "tun:/127.0.0.1:8422?net=198.19.0.102/16&route=198.19.0.0/16,10.233.0.0/16" -f "tcp://127.0.0.1:10800"
type Route struct {
	Listeners []string // -l tun
	Forwarder string   // -f tcp
	Retries   int
}

func (r *Route) ParseForwarder() (*Forwarder, error) {
	forwarder, err := ParseNode(r.Forwarder)
	if err != nil {
		return nil, err
	}
	forwarder.Client = &Client{
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
	}
	return NewForwarder(r.Retries, forwarder), nil
}

func (r *Route) GenerateServers() ([]Server, error) {
	forwarder, err := r.ParseForwarder()
	if err != nil && !errors.Is(err, ErrorInvalidNode) {
		plog.G(context.Background()).Errorf("Failed to parse forwarder: %v", err)
		return nil, err
	}

	servers := make([]Server, 0, len(r.Listeners))
	for _, l := range r.Listeners {
		var node *Node
		node, err = ParseNode(l)
		if err != nil {
			plog.G(context.Background()).Errorf("Failed to parse node %s: %v", l, err)
			return nil, err
		}

		var listener net.Listener
		var handler Handler

		switch node.Protocol {
		case "tun":
			handler = TunHandler(forwarder, node)
			listener, err = tun.Listener(tun.Config{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				Addr6:   node.Get("net6"),
				MTU:     node.GetInt("mtu"),
				Routes:  parseRoutes(node.Get("route")),
				Gateway: node.Get("gw"),
			})
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create tun listener: %v", err)
				return nil, err
			}
		case "tcp":
			handler = TCPHandler()
			listener, err = TCPListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create tcp listener: %v", err)
				return nil, err
			}
		case "gtcp":
			handler = GvisorTCPHandler()
			listener, err = GvisorTCPListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create gvisor tcp listener: %v", err)
				return nil, err
			}
		case "gudp":
			handler = GvisorUDPHandler()
			listener, err = GvisorUDPListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create gvisor udp listener: %v", err)
				return nil, err
			}
		case "ssh":
			handler = SSHHandler()
			listener, err = SSHListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create ssh listener: %v", err)
				return nil, err
			}
		default:
			plog.G(context.Background()).Errorf("Not support protocol %s", node.Protocol)
			return nil, fmt.Errorf("not support protocol %s", node.Protocol)
		}
		servers = append(servers, Server{Listener: listener, Handler: handler})
	}
	return servers, nil
}

func parseRoutes(str string) []types.Route {
	var routes []types.Route
	list := strings.Split(str, ",")
	for _, route := range list {
		if _, ipNet, _ := net.ParseCIDR(strings.TrimSpace(route)); ipNet != nil {
			routes = append(routes, types.Route{Dst: *ipNet})
		}
	}
	return routes
}
