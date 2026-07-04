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
}

func NewBufferedTCP(ctx context.Context, conn net.Conn) net.Conn {
	c := &bufferedTCP{
		Conn: conn,
		ch:   make(chan *DatagramPacket, MaxSize),
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
	c.ch <- newDatagramPacket(buf, n)
	return n, nil
}

func (c *bufferedTCP) run(ctx context.Context) {
	for {
		select {
		case dgram := <-c.ch:
			if dgram == nil {
				return
			}
			_, err := c.Conn.Write(dgram.Data[:dgram.DataLength])
			config.LPool.Put(dgram.Data[:])
			if err != nil {
				plog.G(context.Background()).Errorf("[TCP] Write packet failed: %v", err)
				_ = c.Conn.Close()
				c.closed.Store(true)
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
