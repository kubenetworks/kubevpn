package core

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

var _ net.PacketConn = (*PacketConnOverTCP)(nil)

type PacketConnOverTCP struct {
	// tcp connection
	net.Conn
	ctx context.Context
}

func NewPacketConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &PacketConnOverTCP{ctx: ctx, Conn: conn}, nil
}

func (c *PacketConnOverTCP) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-c.ctx.Done():
		return 0, nil, c.ctx.Err()
	default:
		datagram, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, nil, err
		}
		return int(datagram.DataLength), nil, nil
	}
}

func (c *PacketConnOverTCP) Read(b []byte) (int, error) {
	n, _, err := c.ReadFrom(b)
	return n, err
}

func (c *PacketConnOverTCP) WriteTo(b []byte, _ net.Addr) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}

	buf := config.LPool.Get().([]byte)[:]
	n := copy(buf, b)
	defer config.LPool.Put(buf)

	packet := newDatagramPacket(buf, n)
	if err := packet.Write(c.Conn); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *PacketConnOverTCP) Write(b []byte) (int, error) {
	n, err := c.WriteTo(b, nil)
	return n, err
}

func (c *PacketConnOverTCP) Close() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		_ = cc.CloseRead()
	}
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cc.CloseWrite()
	}
	return c.Conn.Close()
}
