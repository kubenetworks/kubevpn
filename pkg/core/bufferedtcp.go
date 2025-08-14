package core

import (
	"context"
	"errors"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type bufferedTCP struct {
	net.Conn
	Chan   chan *DatagramPacket
	closed bool
}

func NewBufferedTCP(ctx context.Context, conn net.Conn) net.Conn {
	c := &bufferedTCP{
		Conn: conn,
		Chan: make(chan *DatagramPacket, MaxSize),
	}
	go c.Run(ctx)
	return c
}

func (c *bufferedTCP) Write(b []byte) (n int, err error) {
	if c.closed {
		return 0, errors.New("tcp channel is closed")
	}
	if len(b) == 0 {
		return 0, nil
	}

	buf := config.LPool.Get().([]byte)[:]
	n = copy(buf, b)
	c.Chan <- newDatagramPacket(buf, n)
	return n, nil
}

func (c *bufferedTCP) Run(ctx context.Context) {
	for ctx.Err() == nil {
		var buf *DatagramPacket
		select {
		case buf = <-c.Chan:
			if buf == nil {
				return
			}
		case <-ctx.Done():
			return
		}
		_, err := c.Conn.Write(buf.Data[:buf.DataLength])
		config.LPool.Put(buf.Data[:])
		if err != nil {
			plog.G(context.Background()).Errorf("[TCP] Write packet failed: %v", err)
			_ = c.Conn.Close()
			c.closed = true
			return
		}
	}
}

func (c *bufferedTCP) Close() error {
	c.closed = true
	return c.Conn.Close()
}
