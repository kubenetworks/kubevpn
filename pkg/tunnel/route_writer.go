package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
)

// ConnList holds multiple connections for the same client IP, supporting
// write-with-fallback when a connection is dead.
type ConnList struct {
	mu                    sync.Mutex // membership only; never held during I/O
	gateOnce              sync.Once
	writeGate             chan struct{} // raw frame serialization, independent of membership
	members               []*routeMember
	gateStage, writeStage *datapathStage
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
		return framing.WriteFull(conn, data)
	}, routeWriteBytes, cl.containsMember, cl.prepareWriteFailure)
}

// packetWriter is implemented by connections that can take ownership of a pooled,
// reference-counted packet instead of a copied []byte (e.g. bufferedTCP). This lets the
// hot routing paths hand off the framed buffer without the net.Conn.Write copy.
type packetWriter interface {
	// writePacket enqueues an already-framed packet, taking ownership of one reference
	// (the caller must have acquired it). On error ownership is NOT taken.
	writePacket(ctx context.Context, pkt *packet) error
}

// WritePacket writes a packet with reserved length headroom to the first healthy
// conn. For conns that implement packetWriter it transfers one reference
// (zero-copy); otherwise it falls back to net.Conn.Write (which copies). Dead conns are
// removed. The caller retains its own reference and must release it after this returns.
func (cl *ConnList) WritePacket(ctx context.Context, pkt *packet) (net.Conn, error) {
	return cl.writeMembers(ctx, cl.snapshot(), func(conn net.Conn) (int, error) {
		return writeRoutePacket(ctx, conn, pkt)
	}, routeWritePacket, cl.containsMember, cl.prepareWriteFailure)
}

func writeRoutePacket(ctx context.Context, conn net.Conn, pkt *packet) (int, error) {
	if pw, ok := conn.(packetWriter); ok {
		pkt.acquire()
		if err := pw.writePacket(ctx, pkt); err != nil {
			pkt.release()
			return 0, err
		}
		return 0, nil
	}
	return pkt.writeTo(conn)
}

// WriteFunc attempts to call fn on each healthy conn until one succeeds.
// Dead conns (fn returns error) are removed from the list.
func (cl *ConnList) WriteFunc(fn func(conn net.Conn) error) (net.Conn, error) {
	return cl.writeMembers(context.Background(), cl.snapshot(), func(conn net.Conn) (int, error) {
		return 0, fn(conn)
	}, routeWriteCallback, cl.containsMember, cl.prepareWriteFailure)
}

type routeMembersSnapshot struct {
	single *routeMember
	many   []*routeMember
}

func (s routeMembersSnapshot) len() int {
	if s.single != nil {
		return 1
	}
	return len(s.many)
}

func (s routeMembersSnapshot) at(i int) *routeMember {
	if s.single != nil {
		return s.single
	}
	return s.many[i]
}

func (cl *ConnList) snapshot() routeMembersSnapshot {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	switch len(cl.members) {
	case 0:
		return routeMembersSnapshot{}
	case 1:
		return routeMembersSnapshot{single: cl.members[0]}
	default:
		return routeMembersSnapshot{many: append([]*routeMember(nil), cl.members...)}
	}
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
	default:
	}
	w := cl.gateStage.wait()
	ok := false
	defer func() { cl.gateStage.waited(w, ok) }()
	select {
	case cl.writeGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			cl.unlockWrite()
			return err
		}
		ok = true
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

func (cl *ConnList) writeMembers(ctx context.Context, members routeMembersSnapshot, fn func(net.Conn) (int, error), mode routeWriteMode, valid func(*routeMember) bool, prepareFailure func(*routeMember) func()) (net.Conn, error) {
	var lastErr error
	for i := 0; i < members.len(); i++ {
		m := members.at(i)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// packet writers own their serialization; queue admission never holds
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
		var token uint64
		if !buffered {
			token = cl.writeStage.begin()
		}
		n, err := fn(m.conn)
		if !buffered {
			cl.writeStage.finish(token, 1, n, err == nil && ctx.Err() == nil)
		}
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

// routeWriter owns candidate selection, write serialization and failure
// reconciliation. It never performs socket I/O while holding routeTable.mu.
type routeWriter struct {
	table *routeTable
}

func (w *routeWriter) writeToRoutePacket(ctx context.Context, dstKey string, pkt *packet) (net.Conn, error) {
	return w.write(ctx, dstKey, func(conn net.Conn) (int, error) {
		return writeRoutePacket(ctx, conn, pkt)
	}, routeWritePacket)
}

// prepareRawWriteFailure captures identities before I/O. The returned finalizer
// runs only after writeMembers has joined cancellation and closed the transport.
func (w *routeWriter) prepareRawWriteFailure(conn net.Conn) func() {
	type membership struct {
		key    string
		list   *ConnList
		member *routeMember
	}
	var memberships []membership
	hub := w.table
	hub.mu.Lock()
	for key, list := range hub.routes {
		members := list.snapshot()
		for i := 0; i < members.len(); i++ {
			m := members.at(i)
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

func (w *routeWriter) write(ctx context.Context, dstKey string, fn func(net.Conn) (int, error), mode routeWriteMode) (net.Conn, error) {
	hub := w.table
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
			return w.prepareRawWriteFailure(m.conn)
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

var errNoRoute = fmt.Errorf("no route")
