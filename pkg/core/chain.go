package core

import (
	"context"
	"math"
	"net"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

// var (
// 	// errors.New("empty chain") is an error that implies the chain is empty.
// 	errors.New("empty chain") = errors.New("empty chain")
// )

type Chain struct {
	Retries int
	node    *Node
}

func NewChain(retry int, node *Node) *Chain {
	return &Chain{Retries: retry, node: node}
}

func (c *Chain) Node() *Node {
	return c.node
}

func (c *Chain) IsEmpty() bool {
	return c == nil || c.node == nil
}

func (c *Chain) DialContext(ctx context.Context) (conn net.Conn, err error) {
	for i := 0; i < int(math.Max(float64(1), float64(c.Retries))); i++ {
		conn, err = c.dial(ctx)
		if err == nil {
			break
		}
	}
	return
}

func (c *Chain) dial(ctx context.Context) (net.Conn, error) {
	if c.IsEmpty() {
		return nil, errors.New("empty chain")
	}

	conn, err := c.getConn(ctx)
	if err != nil {
		err = errors.Wrap(err, "c.getConn(ctx): ")
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

func (*Chain) resolve(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
			return net.JoinHostPort(ips[0].String(), port)
		}
	}
	return addr
}

func (c *Chain) getConn(ctx context.Context) (net.Conn, error) {
	if c.IsEmpty() {
		return nil, errors.New("empty chain")
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
