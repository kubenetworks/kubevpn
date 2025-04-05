package core

import (
	"context"
	"errors"
	"math"
	"net"
)

var (
	// ErrorEmptyForward is an error that implies the forward is empty.
	ErrorEmptyForward = errors.New("empty forward")
)

type Forward struct {
	retries int
	node    *Node
}

func NewForward(retry int, node *Node) *Forward {
	return &Forward{retries: retry, node: node}
}

func (c *Forward) Node() *Node {
	return c.node
}

func (c *Forward) IsEmpty() bool {
	return c == nil || c.node == nil
}

func (c *Forward) DialContext(ctx context.Context) (conn net.Conn, err error) {
	for i := 0; i < int(math.Max(float64(1), float64(c.retries))); i++ {
		conn, err = c.dial(ctx)
		if err == nil {
			break
		}
	}
	return
}

func (c *Forward) dial(ctx context.Context) (net.Conn, error) {
	if c.IsEmpty() {
		return nil, ErrorEmptyForward
	}

	conn, err := c.getConn(ctx)
	if err != nil {
		return nil, err
	}

	var cc net.Conn
	cc, err = c.Node().Client.ConnectContext(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return cc, nil
}

func (*Forward) resolve(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
			return net.JoinHostPort(ips[0].String(), port)
		}
	}
	return addr
}

func (c *Forward) getConn(ctx context.Context) (net.Conn, error) {
	if c.IsEmpty() {
		return nil, ErrorEmptyForward
	}
	return c.Node().Client.Dial(ctx, c.resolve(c.Node().Addr))
}

type Handler interface {
	Handle(ctx context.Context, conn net.Conn)
}

type Client struct {
	Connector
	Transporter
}

type Connector interface {
	ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error)
}

type Transporter interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)
}

type Server struct {
	Listener net.Listener
	Handler  Handler
}
