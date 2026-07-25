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
	ch     chan *DatagramPacket
	closed atomic.Bool
	done   chan struct{} // closed when run() exits, so Write never blocks on a drainerless channel
}

// NewBufferedTCP wraps a TCP connection with an async write buffer to reduce syscalls.
func NewBufferedTCP(ctx context.Context, conn net.Conn) net.Conn {
	c := &bufferedTCP{
		Conn: conn,
		ch:   make(chan *DatagramPacket, MaxSize),
		done: make(chan struct{}),
	}
	go c.run(ctx)
	return c
}

func (c *bufferedTCP) Write(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, errors.New("tcp channel is closed")
	}
	if len(b) == 0 {
		return 0, nil
	}
	buf := config.LPool.Get().([]byte)
	n = copy(buf, b)
	// Select on done so that if run() has exited (ctx cancelled or write error)
	// we fail fast instead of blocking forever on a channel no one drains.
	select {
	case c.ch <- newDatagramPacket(buf, n):
		return n, nil
	case <-c.done:
		config.LPool.Put(buf[:])
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
		case dgram := <-c.ch:
			if dgram == nil {
				return
			}
			_, err := c.Conn.Write(dgram.Data[:dgram.DataLength])
			config.LPool.Put(dgram.Data[:])
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
		case dgram := <-c.ch:
			config.LPool.Put(dgram.Data[:])
		default:
			return c.Conn.Close()
		}
	}
}
