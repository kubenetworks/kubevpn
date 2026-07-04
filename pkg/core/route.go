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

// ConnList holds multiple connections for the same client IP, supporting
// write-with-fallback when a connection is dead.
type ConnList struct {
	mu    sync.Mutex
	conns []net.Conn
}

// Add appends a conn to the list if not already present.
func (cl *ConnList) Add(conn net.Conn) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for _, c := range cl.conns {
		if c == conn {
			return
		}
	}
	cl.conns = append(cl.conns, conn)
}

// Remove removes a specific conn from the list. Returns true if the list is now empty.
func (cl *ConnList) Remove(conn net.Conn) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for i, c := range cl.conns {
		if c == conn {
			cl.conns = append(cl.conns[:i], cl.conns[i+1:]...)
			break
		}
	}
	return len(cl.conns) == 0
}

// Write attempts to write data to any healthy conn in the list.
// On write failure, the dead conn is removed. Returns the conn that succeeded,
// or an error if all conns failed.
func (cl *ConnList) Write(data []byte) (net.Conn, error) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for i := 0; i < len(cl.conns); i++ {
		_, err := cl.conns[i].Write(data)
		if err == nil {
			return cl.conns[i], nil
		}
		// Write failed — remove dead conn
		cl.conns = append(cl.conns[:i], cl.conns[i+1:]...)
		i--
	}
	return nil, fmt.Errorf("all connections failed")
}

// WriteFunc attempts to call fn on each healthy conn until one succeeds.
// Dead conns (fn returns error) are removed from the list.
func (cl *ConnList) WriteFunc(fn func(conn net.Conn) error) (net.Conn, error) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for i := 0; i < len(cl.conns); i++ {
		if err := fn(cl.conns[i]); err == nil {
			return cl.conns[i], nil
		}
		// Write failed — remove dead conn
		cl.conns = append(cl.conns[:i], cl.conns[i+1:]...)
		i--
	}
	return nil, fmt.Errorf("all connections failed")
}

// Len returns the number of connections.
func (cl *ConnList) Len() int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return len(cl.conns)
}

// IsEmpty returns true if no connections remain.
func (cl *ConnList) IsEmpty() bool {
	return cl.Len() == 0
}

// RouteHub holds the shared routing state between tun and gvisor handlers.
// RouteMapTCP maps srcIP (string) -> *ConnList, enabling multi-conn routing with fallback.
// TCPPacketChan bridges unroutable TCP packets from gvisor endpoint to the tun device.
type RouteHub struct {
	RouteMapTCP   *sync.Map // map[string]*ConnList
	TCPPacketChan chan *Packet
}

// NewRouteHub creates a new RouteHub for multi-client routing.
func NewRouteHub() *RouteHub {
	return &RouteHub{
		RouteMapTCP:   &sync.Map{},
		TCPPacketChan: make(chan *Packet, MaxSize),
	}
}

// DefaultRouteHub is the process-wide shared routing hub.
var DefaultRouteHub = NewRouteHub()

// AddRoute registers a conn for the given source IP.
// Multiple conns can be registered for the same IP (connection pool support).
func (hub *RouteHub) AddRoute(ctx context.Context, srcIP net.IP, conn net.Conn) {
	key := string(srcIP)
	val, loaded := hub.RouteMapTCP.LoadOrStore(key, &ConnList{conns: []net.Conn{conn}})
	if loaded {
		list := val.(*ConnList)
		list.Add(conn)
	} else {
		plog.G(ctx).Infof("[Route] Add route: %s -> %s-%s", srcIP, conn.LocalAddr(), conn.RemoteAddr())
	}
}

// WriteToRoute writes data to the conn(s) registered for dstIP.
// Tries each conn in order; removes dead conns on failure.
// Returns (conn used, nil) on success, or (nil, error) if no route or all conns dead.
func (hub *RouteHub) WriteToRoute(dstKey string, data []byte) (net.Conn, error) {
	val, ok := hub.RouteMapTCP.Load(dstKey)
	if !ok {
		return nil, errNoRoute
	}
	list := val.(*ConnList)
	conn, err := list.Write(data)
	if err != nil {
		// All conns dead — remove the empty entry
		if list.IsEmpty() {
			hub.RouteMapTCP.Delete(dstKey)
		}
		return nil, err
	}
	return conn, nil
}

// WriteFuncToRoute calls fn on available conns for dstKey until one succeeds.
func (hub *RouteHub) WriteFuncToRoute(dstKey string, fn func(conn net.Conn) error) (net.Conn, error) {
	val, ok := hub.RouteMapTCP.Load(dstKey)
	if !ok {
		return nil, errNoRoute
	}
	list := val.(*ConnList)
	conn, err := list.WriteFunc(fn)
	if err != nil {
		if list.IsEmpty() {
			hub.RouteMapTCP.Delete(dstKey)
		}
		return nil, err
	}
	return conn, nil
}

// HasRoute checks if a route exists for the given destination IP key.
func (hub *RouteHub) HasRoute(dstKey string) bool {
	val, ok := hub.RouteMapTCP.Load(dstKey)
	if !ok {
		return false
	}
	return !val.(*ConnList).IsEmpty()
}

// RemoveRoutesByConn removes the given conn from all route entries.
// Cleans up empty entries.
func (hub *RouteHub) RemoveRoutesByConn(ctx context.Context, conn net.Conn) {
	hub.RouteMapTCP.Range(func(key, value any) bool {
		list := value.(*ConnList)
		empty := list.Remove(conn)
		if empty {
			hub.RouteMapTCP.Delete(key)
			plog.G(ctx).Infof("[Route] Remove route: %s (conn %s)", net.IP(key.(string)), conn.LocalAddr())
		}
		return true
	})
}

var errNoRoute = fmt.Errorf("no route")

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
