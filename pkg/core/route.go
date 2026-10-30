package core

import (
	"context"
	"errors"
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
	mu        sync.Mutex // membership only; never held during I/O
	gateOnce  sync.Once
	writeGate chan struct{} // raw frame serialization, independent of membership
	members   []*routeMember
}

// A fresh membership identity prevents an old write from removing a re-added conn.
type routeMember struct {
	conn net.Conn
}

// Add registers a conn at the head of the list if not already present. Returns true if it was added
// (false if it was already registered), so callers can log only genuinely new pool members.
// Prepending makes Write*/WritePacket try the most recently established conn first: after a
// reconnect the fresh conn leads and any lingering half-open "ghost" conn sinks to the tail, where
// it is only a last resort (and is evicted by the read/write deadlines anyway).
func (cl *ConnList) Add(conn net.Conn) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for _, m := range cl.members {
		if m.conn == conn {
			return false
		}
	}
	cl.members = append([]*routeMember{{conn: conn}}, cl.members...)
	return true
}

// Remove removes a specific conn from the list. Returns true if the list is now empty.
func (cl *ConnList) Remove(conn net.Conn) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for i, m := range cl.members {
		if m.conn == conn {
			cl.members = append(cl.members[:i], cl.members[i+1:]...)
			break
		}
	}
	return len(cl.members) == 0
}

// Write attempts to write data to any healthy conn in the list.
// On write failure, the dead conn is removed. Returns the conn that succeeded,
// or an error if all conns failed.
func (cl *ConnList) Write(data []byte) (net.Conn, error) {
	return cl.writeMembers(context.Background(), cl.snapshot(), func(conn net.Conn) (int, error) {
		return writeFull(conn, data)
	}, routeWriteBytes, cl.containsMember, cl.prepareWriteFailure)
}

// packetWriter is implemented by connections that can take ownership of a pooled,
// reference-counted Packet instead of a copied []byte (e.g. bufferedTCP). This lets the
// hot routing paths hand off the framed buffer without the net.Conn.Write copy.
type packetWriter interface {
	// writePacket enqueues an already-framed packet, taking ownership of one reference
	// (the caller must have acquired it). On error ownership is NOT taken.
	writePacket(ctx context.Context, pkt *Packet) error
}

// WritePacket writes an already-framed packet (length header stamped in pkt.data[:2]) to
// the first healthy conn. For conns that implement packetWriter it transfers one reference
// (zero-copy); otherwise it falls back to net.Conn.Write (which copies). Dead conns are
// removed. The caller retains its own reference and must release it after this returns.
func (cl *ConnList) WritePacket(ctx context.Context, pkt *Packet) (net.Conn, error) {
	return cl.writeMembers(ctx, cl.snapshot(), func(conn net.Conn) (int, error) {
		return writeRoutePacket(ctx, conn, pkt)
	}, routeWritePacket, cl.containsMember, cl.prepareWriteFailure)
}

func writeRoutePacket(ctx context.Context, conn net.Conn, pkt *Packet) (int, error) {
	if pw, ok := conn.(packetWriter); ok {
		pkt.acquire()
		if err := pw.writePacket(ctx, pkt); err != nil {
			pkt.release()
			return 0, err
		}
		return 0, nil
	}
	return writeFull(conn, pkt.data[:datagramHeaderLen+pkt.length])
}

// WriteFunc attempts to call fn on each healthy conn until one succeeds.
// Dead conns (fn returns error) are removed from the list.
func (cl *ConnList) WriteFunc(fn func(conn net.Conn) error) (net.Conn, error) {
	return cl.writeMembers(context.Background(), cl.snapshot(), func(conn net.Conn) (int, error) {
		return 0, fn(conn)
	}, routeWriteCallback, cl.containsMember, cl.prepareWriteFailure)
}

func (cl *ConnList) snapshot() []*routeMember {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return append([]*routeMember(nil), cl.members...)
}

func (cl *ConnList) removeMember(member *routeMember) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for i, m := range cl.members {
		if m == member {
			cl.members = append(cl.members[:i], cl.members[i+1:]...)
			break
		}
	}
	return len(cl.members) == 0
}

func (cl *ConnList) containsMember(member *routeMember) bool {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for _, m := range cl.members {
		if m == member {
			return true
		}
	}
	return false
}

func (cl *ConnList) lockWrite(ctx context.Context) error {
	cl.gateOnce.Do(func() { cl.writeGate = make(chan struct{}, 1) })
	select {
	case cl.writeGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			cl.unlockWrite()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (cl *ConnList) unlockWrite() { <-cl.writeGate }

type routeWriteMode uint8

const (
	routeWriteCallback routeWriteMode = iota // arbitrary error; no transport ownership
	routeWriteBytes                          // known byte progress; close on I/O failure
	routeWritePacket                         // packet admission or cancellable raw I/O
)

func (cl *ConnList) prepareWriteFailure(m *routeMember) func() {
	return func() { cl.removeMember(m) }
}

func (cl *ConnList) writeMembers(ctx context.Context, members []*routeMember, fn func(net.Conn) (int, error), mode routeWriteMode, valid func(*routeMember) bool, prepareFailure func(*routeMember) func()) (net.Conn, error) {
	var lastErr error
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Packet writers own their serialization; queue admission never holds
		// the raw gate. Legacy callbacks cannot promise cancellable I/O.
		_, buffered := m.conn.(packetWriter)
		if !buffered {
			if err := cl.lockWrite(ctx); err != nil {
				return nil, err
			}
		}
		if !valid(m) {
			if !buffered {
				cl.unlockWrite()
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			if !buffered {
				cl.unlockWrite()
			}
			return nil, err
		}
		// Capture membership identities before I/O, but reconcile only after
		// the final outcome includes a cancellation callback that won the race.
		failed := prepareFailure(m)
		var stop func() bool
		var closed chan struct{}
		if mode == routeWritePacket && !buffered {
			closed = make(chan struct{})
			stop = context.AfterFunc(ctx, func() {
				_ = m.conn.Close() // no metadata locks held
				close(closed)
			})
		}
		n, err := fn(m.conn)
		canceledWrite := false
		if stop != nil && !stop() {
			<-closed // no late Close can escape this write
			canceledWrite = true
			err = errors.Join(err, ctx.Err())
		}
		// Close a failed known raw write before releasing its serialization gate.
		// The joined cancellation callback already closed it; never close twice.
		if err != nil && !buffered && !canceledWrite && mode != routeWriteCallback {
			_ = m.conn.Close() // no metadata locks held
		}
		// Admission cancellation leaves the buffered connection healthy. Raw
		// failures are reconciled before another waiter can acquire this gate.
		admissionCanceled := mode == routeWritePacket && buffered && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
		if err != nil && !admissionCanceled {
			failed()
		}
		if !buffered {
			cl.unlockWrite()
		}
		if canceledWrite || admissionCanceled {
			return nil, err
		}
		if err == nil {
			return m.conn, nil
		}
		lastErr = err
		if n > 0 {
			return nil, err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("all connections failed")
}

// Len returns the number of connections.
func (cl *ConnList) Len() int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return len(cl.members)
}

// IsEmpty returns true if no connections remain.
func (cl *ConnList) IsEmpty() bool {
	return cl.Len() == 0
}

// RouteHub serializes route membership and stack-lifetime transitions. Lock order
// is hub.mu -> ConnList.mu or handler.mu, never the reverse. Callbacks run under
// hub.mu and may only update state/timers: no I/O, logging, or hub reentry.
type RouteHub struct {
	mu          sync.Mutex
	routes      map[string]*ConnList
	tunQueue    chan *Packet
	tunDone     chan struct{}
	tunStopOnce sync.Once
	tunEnqueue  sync.RWMutex
	emptyLocked func(ipKey string)
	addedLocked func(ipKey string)
}

// NewRouteHub creates a new RouteHub for multi-client routing.
func NewRouteHub() *RouteHub {
	return &RouteHub{
		routes:   make(map[string]*ConnList),
		tunQueue: make(chan *Packet, MaxSize),
		tunDone:  make(chan struct{}),
	}
}

// enqueueTUN transfers one reference on success and none on failure. The data
// channel is never closed; stop joins even a send racing with cancellation.
func (hub *RouteHub) enqueueTUN(ctx context.Context, pkt *Packet) bool {
	hub.tunEnqueue.RLock()
	defer hub.tunEnqueue.RUnlock()
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-hub.tunDone:
		return false
	default:
	}
	select {
	case hub.tunQueue <- pkt:
		return true
	case <-hub.tunDone:
		return false
	case <-ctx.Done():
		return false
	}
}

// stopTUNBridge is terminal for this device bridge, not for the route table.
func (hub *RouteHub) stopTUNBridge() {
	hub.tunStopOnce.Do(func() {
		close(hub.tunDone)
		hub.tunEnqueue.Lock()
		drainPacketChan(hub.tunQueue)
		hub.tunEnqueue.Unlock()
	})
}

// AddRoute registers a conn for the given source IP.
// Multiple conns can be registered for the same IP (connection pool support).
// False rejects a stopped buffered connection; its reader must terminate.
func (hub *RouteHub) AddRoute(ctx context.Context, srcIP net.IP, conn net.Conn) bool {
	key := string(srcIP)
	hub.mu.Lock()
	if c, ok := conn.(*bufferedTCP); ok && c.closed.Load() {
		hub.mu.Unlock()
		return false
	}
	list := hub.routes[key]
	created := list == nil
	if created {
		list = &ConnList{}
		hub.routes[key] = list
	}
	added := list.Add(conn)
	n := list.Len()
	if created && hub.addedLocked != nil {
		hub.addedLocked(key)
	}
	hub.mu.Unlock()
	if created {
		plog.G(ctx).Debugf("[Route] Add route: %s -> %s-%s", srcIP, conn.LocalAddr(), conn.RemoteAddr())
	} else if added {
		plog.G(ctx).Debugf("[Route] Add pool conn: %s -> %s-%s (now %d conns)", srcIP, conn.LocalAddr(), conn.RemoteAddr(), n)
	}
	return true
}

// WriteToRoute writes data to the conn(s) registered for dstIP.
// Tries each conn in order; removes dead conns on failure.
// Returns (conn used, nil) on success, or (nil, error) if no route or all conns dead.
func (hub *RouteHub) WriteToRoute(dstKey string, data []byte) (net.Conn, error) {
	return hub.writeToRoute(context.Background(), dstKey, func(conn net.Conn) (int, error) {
		return writeFull(conn, data)
	}, routeWriteBytes)
}

// prepareRawWriteFailure captures identities before I/O. The returned finalizer
// runs only after writeMembers has joined cancellation and closed the transport.
func (hub *RouteHub) prepareRawWriteFailure(conn net.Conn) func() {
	type membership struct {
		key    string
		list   *ConnList
		member *routeMember
	}
	var memberships []membership
	hub.mu.Lock()
	for key, list := range hub.routes {
		for _, m := range list.snapshot() {
			if m.conn == conn {
				memberships = append(memberships, membership{key, list, m})
			}
		}
	}
	hub.mu.Unlock()
	return func() {
		hub.mu.Lock()
		for _, m := range memberships {
			if hub.routes[m.key] == m.list && m.list.removeMember(m.member) {
				hub.deleteEmptyLocked(m.key)
			}
		}
		hub.mu.Unlock()
	}
}

// WriteFuncToRoute calls fn on available conns for dstKey until one succeeds.
func (hub *RouteHub) WriteFuncToRoute(dstKey string, fn func(conn net.Conn) error) (net.Conn, error) {
	return hub.writeToRoute(context.Background(), dstKey, func(conn net.Conn) (int, error) {
		return 0, fn(conn)
	}, routeWriteCallback)
}

func (hub *RouteHub) writeToRoute(ctx context.Context, dstKey string, fn func(net.Conn) (int, error), mode routeWriteMode) (net.Conn, error) {
	hub.mu.Lock()
	list := hub.routes[dstKey]
	if list == nil {
		hub.mu.Unlock()
		return nil, errNoRoute
	}
	members := list.snapshot()
	hub.mu.Unlock()
	return list.writeMembers(ctx, members, fn, mode, func(m *routeMember) bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return hub.routes[dstKey] == list && list.containsMember(m)
	}, func(m *routeMember) func() {
		if _, buffered := m.conn.(packetWriter); !buffered && mode != routeWriteCallback {
			return hub.prepareRawWriteFailure(m.conn)
		}
		return func() {
			hub.mu.Lock()
			defer hub.mu.Unlock()
			if hub.routes[dstKey] == list && list.removeMember(m) {
				hub.deleteEmptyLocked(dstKey)
			}
		}
	})
}

// WriteToRoutePacket writes an already-framed packet to the conn(s) registered for dstKey,
// transferring ownership of one reference to the chosen conn (zero-copy via packetWriter).
// The caller retains its own reference and must release it afterward.
func (hub *RouteHub) WriteToRoutePacket(ctx context.Context, dstKey string, pkt *Packet) (net.Conn, error) {
	return hub.writeToRoute(ctx, dstKey, func(conn net.Conn) (int, error) {
		return writeRoutePacket(ctx, conn, pkt)
	}, routeWritePacket)
}

// HasRoute checks if a route exists for the given destination IP key.
func (hub *RouteHub) HasRoute(dstKey string) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.routes[dstKey] != nil
}

// RemoveRoutesByConn removes the given conn from all route entries.
// Cleans up empty entries.
func (hub *RouteHub) RemoveRoutesByConn(ctx context.Context, conn net.Conn) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for key, list := range hub.routes {
		if list.Remove(conn) {
			hub.deleteEmptyLocked(key)
		}
	}
}

func (hub *RouteHub) deleteEmptyLocked(key string) {
	delete(hub.routes, key)
	if hub.emptyLocked != nil {
		hub.emptyLocked(key)
	}
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
