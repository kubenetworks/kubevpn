package core

import (
	"context"
	"errors"
	"net"
)

var (
	// ErrorEmptyForwarder is an error that implies the forward is empty.
	ErrorEmptyForwarder = errors.New("empty forwarder")
)

type Forwarder struct {
	retries int
	node    *Node
}

func NewForwarder(retry int, node *Node) *Forwarder {
	return &Forwarder{retries: retry, node: node}
}

func (c *Forwarder) Node() *Node {
	return c.node
}

func (c *Forwarder) IsEmpty() bool {
	return c == nil || c.node == nil
}

func (c *Forwarder) DialContext(ctx context.Context) (conn net.Conn, err error) {
	for i := 0; i < max(1, c.retries); i++ {
		conn, err = c.dial(ctx)
		if err == nil {
			break
		}
	}
	return
}

func (c *Forwarder) dial(ctx context.Context) (net.Conn, error) {
	if c.IsEmpty() {
		return nil, ErrorEmptyForwarder
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

func (*Forwarder) resolve(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
			return net.JoinHostPort(ips[0].String(), port)
		}
	}
	return addr
}

func (c *Forwarder) getConn(ctx context.Context) (net.Conn, error) {
	if c.IsEmpty() {
		return nil, ErrorEmptyForwarder
	}
	return c.Node().Client.Dial(ctx, c.Node().Addr)
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
