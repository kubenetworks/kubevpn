package core

import (
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel"
)

// ProtocolFactory creates a listener and handler for a given protocol node.
type ProtocolFactory func(node *Node, hub *tunnel.Router) (net.Listener, Handler, error)

var protocolRegistry = map[string]ProtocolFactory{}

// RegisterProtocol registers a factory for the given protocol name.
func RegisterProtocol(name string, factory ProtocolFactory) {
	protocolRegistry[name] = factory
}

// GenerateServers creates servers from listener URI strings (for CLI/config input).
func GenerateServers(listeners []string, hub *tunnel.Router) ([]Server, error) {
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
// If hub is nil, a new tunnel.Router is created automatically.
func GenerateServersFromNodes(nodes []*Node, hub *tunnel.Router) ([]Server, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no listeners configured")
	}
	if hub == nil {
		hub = NewRouteHub()
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

func NewRouteHub() *tunnel.Router { return tunnel.NewRouter() }
