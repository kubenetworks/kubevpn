package tunnel

import (
	"context"
	"net"
	"sync"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// routeTable serializes route membership and peer transitions in one transaction.
// Lock order is routeTable.mu -> membership/peer mutex; no I/O occurs under these locks.
type routeTable struct {
	mu     sync.Mutex
	routes map[string]*ConnList
	peers  *peerRegistry
}

func (hub *routeTable) addRoute(ctx context.Context, srcIP net.IP, conn net.Conn) bool {
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
		routeCtx := context.WithoutCancel(ctx)
		list.gateStage = diagnosticStage(routeCtx, "raw_write_gate", "server", -1, 0)
		list.writeStage = diagnosticStage(routeCtx, "raw_socket_write", "server", -1, 0)
		hub.routes[key] = list
	}
	added := list.Add(conn)
	n := list.Len()
	if created {
		hub.peers.onRouteAddedLocked(key)
	}
	hub.mu.Unlock()
	if added {
		list.gateStage.setPeer(srcIP)
		list.writeStage.setPeer(srcIP)
		if c, ok := conn.(*bufferedTCP); ok {
			c.enqueueStage.setPeer(srcIP)
			c.writeStage.setPeer(srcIP)
		}
	}
	if created {
		plog.G(ctx).Debugf("[Route] Add route: %s -> %s-%s", srcIP, conn.LocalAddr(), conn.RemoteAddr())
	} else if added {
		plog.G(ctx).Debugf("[Route] Add pool conn: %s -> %s-%s (now %d conns)", srcIP, conn.LocalAddr(), conn.RemoteAddr(), n)
	}
	return true
}

func (hub *routeTable) hasRoute(dstKey string) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.routes[dstKey] != nil
}

func (hub *routeTable) removeRoutesByConn(ctx context.Context, conn net.Conn) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for key, list := range hub.routes {
		if list.Remove(conn) {
			hub.deleteEmptyLocked(key)
		}
	}
}

func (hub *routeTable) deleteEmptyLocked(key string) {
	if list := hub.routes[key]; list != nil {
		list.gateStage.close()
		list.writeStage.close()
	}
	delete(hub.routes, key)
	hub.peers.onRouteEmptyLocked(key)
}

// AddRoute registers a conn for the given source IP. Multiple conns can share
// one route; a stopped buffered connection is rejected.
func (hub *Router) AddRoute(ctx context.Context, srcIP net.IP, conn net.Conn) bool {
	return hub.routeTable.addRoute(ctx, srcIP, conn)
}

// HasRoute reports whether any connection is registered for dstKey.
func (hub *Router) HasRoute(dstKey string) bool {
	return hub.routeTable.hasRoute(dstKey)
}

// RemoveRoutesByConn removes conn from every route and cleans up empty entries.
func (hub *Router) RemoveRoutesByConn(ctx context.Context, conn net.Conn) {
	hub.routeTable.removeRoutesByConn(ctx, conn)
}

// WriteToRoute writes already-framed raw bytes with zero-progress fallback.
func (hub *Router) WriteToRoute(dstKey string, data []byte) (net.Conn, error) {
	return hub.writer.writeToRoute(dstKey, data)
}

// WriteFuncToRoute calls fn on available conns until one succeeds.
func (hub *Router) WriteFuncToRoute(dstKey string, fn func(conn net.Conn) error) (net.Conn, error) {
	return hub.writer.writeFuncToRoute(dstKey, fn)
}

// WriteToRoutePacket borrows the caller's packet and transfers one acquired
// reference to an asynchronous packet writer when one is selected.
func (hub *Router) WriteToRoutePacket(ctx context.Context, dstKey string, pkt *packet) (net.Conn, error) {
	return hub.writer.writeToRoutePacket(ctx, dstKey, pkt)
}
