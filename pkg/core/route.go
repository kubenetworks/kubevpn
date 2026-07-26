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

// Add appends a conn to the list if not already present. Returns true if it was added
// (false if it was already registered), so callers can log only genuinely new pool members.
func (cl *ConnList) Add(conn net.Conn) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for _, c := range cl.conns {
		if c == conn {
			return false
		}
	}
	cl.conns = append(cl.conns, conn)
	return true
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

// packetWriter is implemented by connections that can take ownership of a pooled,
// reference-counted Packet instead of a copied []byte (e.g. bufferedTCP). This lets the
// hot routing paths hand off the framed buffer without the net.Conn.Write copy.
type packetWriter interface {
	// writePacket enqueues an already-framed packet, taking ownership of one reference
	// (the caller must have acquired it). Returns false if the conn is closed, in which
	// case ownership is NOT taken and the caller keeps its reference.
	writePacket(pkt *Packet) bool
}

// WritePacket writes an already-framed packet (length header stamped in pkt.data[:2]) to
// the first healthy conn. For conns that implement packetWriter it transfers one reference
// (zero-copy); otherwise it falls back to net.Conn.Write (which copies). Dead conns are
// removed. The caller retains its own reference and must release it after this returns.
func (cl *ConnList) WritePacket(pkt *Packet) (net.Conn, error) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for i := 0; i < len(cl.conns); i++ {
		if pw, ok := cl.conns[i].(packetWriter); ok {
			pkt.acquire()
			if pw.writePacket(pkt) {
				return cl.conns[i], nil
			}
			pkt.release() // conn closed, ownership not taken — undo and try next
		} else if _, err := cl.conns[i].Write(pkt.data[:datagramHeaderLen+pkt.length]); err == nil {
			return cl.conns[i], nil
		}
		// Dead conn — remove it.
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

// AddRoute registers a conn for the given source IP.
// Multiple conns can be registered for the same IP (connection pool support).
func (hub *RouteHub) AddRoute(ctx context.Context, srcIP net.IP, conn net.Conn) {
	key := string(srcIP)
	val, loaded := hub.RouteMapTCP.LoadOrStore(key, &ConnList{conns: []net.Conn{conn}})
	if loaded {
		list := val.(*ConnList)
		if list.Add(conn) {
			plog.G(ctx).Infof("[Route] Add pool conn: %s -> %s-%s (now %d conns)", srcIP, conn.LocalAddr(), conn.RemoteAddr(), list.Len())
		}
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
		// All conns dead — remove the empty entry.
		// TOCTOU between IsEmpty and Delete is harmless: if another goroutine
		// added a conn between the two calls, Delete removes it but the next
		// packet will re-register the route via AddRoute. sync.Map.Delete is idempotent.
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
		// Same harmless TOCTOU as WriteToRoute — see comment there.
		if list.IsEmpty() {
			hub.RouteMapTCP.Delete(dstKey)
		}
		return nil, err
	}
	return conn, nil
}

// WriteToRoutePacket writes an already-framed packet to the conn(s) registered for dstKey,
// transferring ownership of one reference to the chosen conn (zero-copy via packetWriter).
// The caller retains its own reference and must release it afterward.
func (hub *RouteHub) WriteToRoutePacket(dstKey string, pkt *Packet) (net.Conn, error) {
	val, ok := hub.RouteMapTCP.Load(dstKey)
	if !ok {
		return nil, errNoRoute
	}
	list := val.(*ConnList)
	conn, err := list.WritePacket(pkt)
	if err != nil {
		// Same harmless TOCTOU as WriteToRoute — see comment there.
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
// If hub is nil, a new RouteHub is created automatically.
func GenerateServersFromNodes(nodes []*Node, hub *RouteHub) ([]Server, error) {
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
