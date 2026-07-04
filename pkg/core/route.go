package core

import (
	"context"
	"fmt"
	"net"
	"sync"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ProtocolFactory creates a listener and handler for a given protocol node.
type ProtocolFactory func(node *Node, hub *RouteHub) (net.Listener, Handler, error)

var protocolRegistry = map[string]ProtocolFactory{}

// RegisterProtocol registers a factory for the given protocol name.
func RegisterProtocol(name string, factory ProtocolFactory) {
	protocolRegistry[name] = factory
}

// RouteHub holds the shared routing state between tun and gvisor handlers.
// RouteMapTCP maps srcIP (string) -> net.Conn, enabling cross-handler route discovery.
// TCPPacketChan bridges unroutable TCP packets from gvisor endpoint to the tun device.
type RouteHub struct {
	RouteMapTCP   *sync.Map
	TCPPacketChan chan *Packet
}

func NewRouteHub() *RouteHub {
	return &RouteHub{
		RouteMapTCP:   &sync.Map{},
		TCPPacketChan: make(chan *Packet, MaxSize),
	}
}

// DefaultRouteHub is the process-wide shared routing hub.
var DefaultRouteHub = NewRouteHub()

// AddRoute registers or updates the TCP route for a source IP.
func (hub *RouteHub) AddRoute(ctx context.Context, srcIP net.IP, conn net.Conn) {
	key := srcIP.String()
	value, loaded := hub.RouteMapTCP.LoadOrStore(key, conn)
	if loaded {
		if value.(net.Conn) != conn {
			hub.RouteMapTCP.Store(key, conn)
			plog.G(ctx).Infof("[RouteHub] Replace route: %s -> %s-%s", srcIP, conn.LocalAddr(), conn.RemoteAddr())
		}
	} else {
		plog.G(ctx).Infof("[RouteHub] Add route: %s -> %s-%s", srcIP, conn.LocalAddr(), conn.RemoteAddr())
	}
}

// RemoveRoutesByConn removes all TCP routes associated with the given connection.
func (hub *RouteHub) RemoveRoutesByConn(ctx context.Context, conn net.Conn) {
	hub.RouteMapTCP.Range(func(key, value any) bool {
		if value.(net.Conn) == conn {
			hub.RouteMapTCP.Delete(key)
			plog.G(ctx).Infof("[RouteHub] Remove route: %s (conn %s)", key, conn.LocalAddr())
		}
		return true
	})
}

// GenerateServers creates servers from listener URI strings (for CLI/config input).
func GenerateServers(listeners []string, hub *RouteHub) ([]Server, error) {
	if len(listeners) == 0 {
		return nil, fmt.Errorf("no listeners configured")
	}
	nodes := make([]*Node, 0, len(listeners))
	for _, l := range listeners {
		node, err := ParseNode(l)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", l, err)
		}
		nodes = append(nodes, node)
	}
	return GenerateServersFromNodes(nodes, hub)
}

// GenerateServersFromNodes creates servers from pre-built Node objects.
func GenerateServersFromNodes(nodes []*Node, hub *RouteHub) ([]Server, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no listeners configured")
	}
	if hub == nil {
		hub = DefaultRouteHub
	}
	servers := make([]Server, 0, len(nodes))
	for _, node := range nodes {
		factory, ok := protocolRegistry[node.Protocol]
		if !ok {
			return nil, fmt.Errorf("unsupported protocol %q", node.Protocol)
		}
		ln, handler, err := factory(node, hub)
		if err != nil {
			return nil, fmt.Errorf("create %s server: %w", node.Protocol, err)
		}
		servers = append(servers, Server{Listener: ln, Handler: handler})
	}
	return servers, nil
}
