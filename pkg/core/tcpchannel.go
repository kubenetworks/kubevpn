package core

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type tcpChan struct {
	net.Conn
	Chan   chan *DatagramPacket
	once   sync.Once
	closed bool
}

func NewTCPChan(conn net.Conn) net.Conn {
	c := &tcpChan{
		Conn: conn,
		Chan: make(chan *DatagramPacket, MaxSize),
	}
	go c.Run()
	return c
}

func (c *tcpChan) Write(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, err
	}
	if c.closed {
		return 0, errors.New("tcp channel is closed")
	}

	buf := config.LPool.Get().([]byte)[:]
	n = copy(buf, b)
	c.Chan <- &DatagramPacket{
		DataLength: uint16(n),
		Data:       buf,
	}
	return n, nil
}

func (c *tcpChan) Run() {
	for buf := range c.Chan {
		_, err := c.Conn.Write(buf.Data[:buf.DataLength])
		config.LPool.Put(buf.Data[:])
		if err != nil {
			plog.G(context.Background()).Errorf("[TCP] Write packet failed: %v", err)
			c.once.Do(func() {
				_ = c.Conn.Close()
				c.closed = true
			})
		}
	}
}
