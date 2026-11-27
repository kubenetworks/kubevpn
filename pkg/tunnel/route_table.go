package tunnel

import (
	"context"
	"net"
	"sync"

	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// routeTable serializes route membership and peer transitions in one transaction.
// Lock order is routeTable.mu -> membership/peer mutex; no I/O occurs under these locks.
type routeTable struct {
	mu     sync.Mutex
	routes map[string]*ConnList
	peers  *peerRegistry
	// reserved holds hub-owned IPs (the traffic manager's own Service ClusterIP), keyed
	// like route entries (routeIPKey). Routes are never learned for them and packets
	// destined to them are always handled by the local gvisor forwarder — see
	// addRoute and dispatchServerDataPacket.
	reserved map[string]struct{}
}

// routeIPKey normalizes ip to the same byte form route keys use: ParseIPFast yields a
// 4-byte slice for IPv4 and a 16-byte slice for IPv6, so the reserved-set keys must be
// built the same way to match the string(src)/string(dst) lookups.
func routeIPKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return string(v4)
	}
	return string(ip.To16())
}

// ReserveSelfIPs marks IPs the hub itself terminates (the traffic manager's own Service
// ClusterIP and any other address its dial-out forwarders answer on).
//
// Failure prevented: the route-table self-registration loop. Routes are learned from
// packet source IPs (addRoute), and a client's gvisor stack answers a bounced packet
// with the bounced destination as its source — once that destination is the hub's own
// ClusterIP, the learned route hijacks every client's flow to the ClusterIP (DNS :53,
// xDS :9002) away from the dial-out forwarder and bounces it back to clients, which
// re-sends spoofed replies and keeps the loop alive. Black-holes the ClusterIP for the
// whole connection lifetime.
func (hub *Router) ReserveSelfIPs(ips ...net.IP) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.reserved == nil {
		hub.reserved = make(map[string]struct{})
	}
	for _, ip := range ips {
		if ip == nil {
			continue
		}
		hub.reserved[routeIPKey(ip)] = struct{}{}
	}
}

// isReserved reports whether key (an already-normalized route key) is a hub-owned IP.
func (hub *routeTable) isReserved(key string) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	_, ok := hub.reserved[key]
	return ok
}

func (hub *routeTable) addRoute(ctx context.Context, srcIP net.IP, conn net.Conn) bool {
	key := string(srcIP)
	hub.mu.Lock()
	// Guard: never learn a route for a hub-owned IP (e.g. the traffic manager's own
	// Service ClusterIP). A packet carrying one as its source is a spoofed reply — a
	// client gvisor stack answering a packet the data path bounced at it. Registering
	// it would route every client's flow to the ClusterIP back to that client instead
	// of dialing out (the route-table self-registration loop that black-holes
	// ClusterIP-destined DNS and xDS). Return true so the caller's read loop keeps
	// running; the packet is processed normally, just without a route entry.
	if _, ok := hub.reserved[key]; ok {
		hub.mu.Unlock()
		plog.G(ctx).Warnf("[Route] Refusing to learn route for hub-owned IP %s from %s (spoofed source)", srcIP, conn.RemoteAddr())
		return true
	}
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
	return hub.writer.write(context.Background(), dstKey, func(conn net.Conn) (int, error) {
		return framing.WriteFull(conn, data)
	}, routeWriteBytes)
}

// WriteFuncToRoute calls fn on available conns until one succeeds.
func (hub *Router) WriteFuncToRoute(dstKey string, fn func(conn net.Conn) error) (net.Conn, error) {
	return hub.writer.write(context.Background(), dstKey, func(conn net.Conn) (int, error) {
		return 0, fn(conn)
	}, routeWriteCallback)
}

// WriteToRoutePacket borrows the caller's packet and transfers one acquired
// reference to an asynchronous packet writer when one is selected.
func (hub *Router) WriteToRoutePacket(ctx context.Context, dstKey string, pkt *packet) (net.Conn, error) {
	return hub.writer.write(ctx, dstKey, func(conn net.Conn) (int, error) {
		return writeRoutePacket(ctx, conn, pkt)
	}, routeWritePacket)
}
