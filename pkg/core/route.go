package core

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tun"
)

var (
	// RouteNAT Globe route table for inner ip
	RouteNAT = NewNAT()
	// RouteConnNAT map[srcIP]net.Conn
	RouteConnNAT = &sync.Map{}
	// Chan tcp connects
	Chan = make(chan *datagramPacket, MaxSize)
)

type TCPUDPacket struct {
	data *datagramPacket
}

// Route example:
// -L "tcp://:10800" -L "tun://:8422?net=223.254.0.100/16"
// -L "tun:/10.233.24.133:8422?net=223.254.0.102/16&route=223.254.0.0/16"
// -L "tun:/127.0.0.1:8422?net=223.254.0.102/16&route=223.254.0.0/16,10.233.0.0/16" -F "tcp://127.0.0.1:10800"
type Route struct {
	ServeNodes []string // -L tun
	ChainNode  string   // -F tcp
	Retries    int
}

func (r *Route) parseChain() (*Chain, error) {
	// parse the base nodes
	node, err := parseChainNode(r.ChainNode)
	if err != nil {
		return nil, err
	}
	return NewChain(r.Retries, node), nil
}

func parseChainNode(ns string) (*Node, error) {
	node, err := ParseNode(ns)
	if err != nil {
		log.Errorf("parse node error: %v", err)
		return nil, err
	}
	node.Client = &Client{
		Connector:   UDPOverTCPTunnelConnector(),
		Transporter: TCPTransporter(),
	}
	return node, nil
}

func (r *Route) GenerateServers() ([]Server, error) {
	chain, err := r.parseChain()
	if err != nil && !errors.Is(err, ErrorInvalidNode) {
		log.Errorf("parse chain error: %v", err)
		return nil, err
	}

	servers := make([]Server, 0, len(r.ServeNodes))
	for _, serveNode := range r.ServeNodes {
		var node *Node
		node, err = ParseNode(serveNode)
		if err != nil {
			log.Errorf("parse node %s error: %v", serveNode, err)
			return nil, err
		}

		var ln net.Listener
		var handler Handler

		switch node.Protocol {
		case "tun":
			handler = TunHandler(chain, node)
			ln, err = tun.Listener(tun.Config{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				Addr6:   os.Getenv(config.EnvInboundPodTunIPv6),
				MTU:     node.GetInt("mtu"),
				Routes:  parseIPRoutes(node.Get("route")),
				Gateway: node.Get("gw"),
			})
			if err != nil {
				log.Errorf("create tun listener error: %v", err)
				return nil, err
			}
		case "tcp":
			handler = TCPHandler()
			ln, err = TCPListener(node.Addr)
			if err != nil {
				log.Errorf("create tcp listener error: %v", err)
				return nil, err
			}
		case "gtcp":
			handler = GvisorTCPHandler()
			ln, err = GvisorTCPListener(node.Addr)
			if err != nil {
				log.Errorf("create gvisor tcp listener error: %v", err)
				return nil, err
			}
		case "gudp":
			handler = GvisorUDPHandler()
			ln, err = GvisorUDPListener(node.Addr)
			if err != nil {
				log.Errorf("create gvisor udp listener error: %v", err)
				return nil, err
			}
		default:
			log.Errorf("not support protocol %s", node.Protocol)
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
