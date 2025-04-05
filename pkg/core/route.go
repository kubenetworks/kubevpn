package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

var (
	// RouteMapTCP map[srcIP]net.Conn Globe route table for inner ip
	RouteMapTCP = &sync.Map{}
	// TCPPacketChan tcp connects
	TCPPacketChan = make(chan *DatagramPacket, MaxSize)
)

type TCPUDPacket struct {
	data *DatagramPacket
}

// Route example:
// -l "tcp://:10800" -l "tun://:8422?net=198.19.0.100/16"
// -l "tun:/10.233.24.133:8422?net=198.19.0.102/16&route=198.19.0.0/16"
// -l "tun:/127.0.0.1:8422?net=198.19.0.102/16&route=198.19.0.0/16,10.233.0.0/16" -f "tcp://127.0.0.1:10800"
type Route struct {
	Listeners []string // -l tun
	Forward   string   // -f tcp
	Retries   int
}

func (r *Route) parseForward() (*Forward, error) {
	node, err := ParseNode(r.Forward)
	if err != nil {
		return nil, err
	}
	node.Client = &Client{
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(),
	}
	return NewForward(r.Retries, node), nil
}

func (r *Route) GenerateServers() ([]Server, error) {
	forward, err := r.parseForward()
	if err != nil && !errors.Is(err, ErrorInvalidNode) {
		plog.G(context.Background()).Errorf("Failed to parse forward: %v", err)
		return nil, err
	}

	servers := make([]Server, 0, len(r.Listeners))
	for _, serveNode := range r.Listeners {
		var node *Node
		node, err = ParseNode(serveNode)
		if err != nil {
			plog.G(context.Background()).Errorf("Failed to parse node %s: %v", serveNode, err)
			return nil, err
		}

		var ln net.Listener
		var handler Handler

		switch node.Protocol {
		case "tun":
			handler = TunHandler(forward, node)
			ln, err = tun.Listener(tun.Config{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				Addr6:   os.Getenv(config.EnvInboundPodTunIPv6),
				MTU:     node.GetInt("mtu"),
				Routes:  parseIPRoutes(node.Get("route")),
				Gateway: node.Get("gw"),
			})
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create tun listener: %v", err)
				return nil, err
			}
		case "tcp":
			handler = TCPHandler()
			ln, err = TCPListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create tcp listener: %v", err)
				return nil, err
			}
		case "gtcp":
			handler = GvisorTCPHandler()
			ln, err = GvisorTCPListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create gvisor tcp listener: %v", err)
				return nil, err
			}
		case "gudp":
			handler = GvisorUDPHandler()
			ln, err = GvisorUDPListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create gvisor udp listener: %v", err)
				return nil, err
			}
		case "ssh":
			handler = SSHHandler()
			ln, err = SSHListener(node.Addr)
			if err != nil {
				plog.G(context.Background()).Errorf("Failed to create ssh listener: %v", err)
				return nil, err
			}
		default:
			plog.G(context.Background()).Errorf("Not support protocol %s", node.Protocol)
			return nil, fmt.Errorf("not support protocol %s", node.Protocol)
		}
		servers = append(servers, Server{Listener: ln, Handler: handler})
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
