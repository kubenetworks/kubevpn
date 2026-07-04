package core

import (
	"context"
	"errors"
	"net"
	"sync/atomic"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type bufferedTCP struct {
	net.Conn
	ch     chan *Packet
	closed atomic.Bool
	done   chan struct{} // closed when run() exits, so Write never blocks on a drainerless channel
}

// NewBufferedTCP wraps a TCP connection with an async write buffer to reduce syscalls.
func NewBufferedTCP(ctx context.Context, conn net.Conn) net.Conn {
	c := &bufferedTCP{
		Conn: conn,
		ch:   make(chan *Packet, MaxSize),
		done: make(chan struct{}),
	}
	go c.run(ctx)
	return c
}

// writePacket enqueues an already-framed packet (length header stamped in pkt.data[:2]),
// taking ownership of one reference: run() releases it after the socket write. Returns
// false if the conn is closed, in which case ownership is NOT taken (caller keeps its ref).
func (c *bufferedTCP) writePacket(pkt *Packet) bool {
	if c.closed.Load() {
		return false
	}
	select {
	case c.ch <- pkt:
		return true
	case <-c.done:
		return false
	}
}

// Write satisfies net.Conn. b must be a fully framed datagram ([2-byte length][payload]).
// Because the net.Conn contract lets the caller reuse b after Write returns, b is copied
// into a pooled buffer. Hot routing paths use writePacket instead to avoid this copy.
func (c *bufferedTCP) Write(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, errors.New("tcp channel is closed")
	}
	if len(b) < datagramHeaderLen {
		return 0, nil
	}
	buf := config.LPool.Get().([]byte)
	n = copy(buf, b)
	pkt := NewPacket(buf, n-datagramHeaderLen, nil, nil)
	// Select on done so that if run() has exited (ctx cancelled or write error)
	// we fail fast instead of blocking forever on a channel no one drains.
	select {
	case c.ch <- pkt:
		return n, nil
	case <-c.done:
		pkt.release()
		return 0, errors.New("tcp channel is closed")
	}
}

func (c *bufferedTCP) run(ctx context.Context) {
	// Mark closed and signal Write before returning, regardless of exit path,
	// so a concurrent Write cannot enqueue into a channel that will never drain.
	defer func() {
		c.closed.Store(true)
		close(c.done)
	}()
	for {
		select {
		case pkt := <-c.ch:
			if pkt == nil {
				return
			}
			_, err := c.Conn.Write(pkt.data[:datagramHeaderLen+pkt.length])
			pkt.release()
			if err != nil {
				plog.G(ctx).Errorf("[TCP] Failed to write packet: %v", err)
				_ = c.Conn.Close()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *bufferedTCP) Close() error {
	c.closed.Store(true)
	for {
		select {
		case pkt := <-c.ch:
			pkt.release()
		default:
			return c.Conn.Close()
		}
	}
}
