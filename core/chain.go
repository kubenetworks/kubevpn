package core

import (
	"context"
	"errors"
	"fmt"
	"github.com/wencaiwulue/kubevpn/util"
	"net"
)

var (
	// ErrEmptyChain is an error that implies the chain is empty.
	ErrEmptyChain = errors.New("empty chain")
)

// Chain is a proxy chain that holds a list of proxy node groups.
type Chain struct {
	Retries int
	node    *Node
}

// NewChain creates a proxy chain with a list of proxy nodes.
// It creates the node groups automatically, one group per node.
func NewChain(retry int, node *Node) *Chain {
	return &Chain{Retries: retry, node: node}
}

func (c *Chain) Node() *Node {
	return c.node
}

// IsEmpty checks if the chain is empty.
// An empty chain means that there is no proxy node or node group in the chain.
func (c *Chain) IsEmpty() bool {
	return c == nil || c.node == nil
}

// DialContext connects to the address on the named network using the provided context.
func (c *Chain) DialContext(ctx context.Context, network, address string) (conn net.Conn, err error) {
	retries := 1
	if c != nil && c.Retries > 0 {
		retries = c.Retries
	}

	for i := 0; i < retries; i++ {
		conn, err = c.dial(ctx, network, address)
		if err == nil {
			break
		}
	}
	return
}

func (c *Chain) dial(ctx context.Context, network, address string) (net.Conn, error) {
	ipAddr := address
	if address != "" {
		ipAddr = c.resolve(address)
	}

	if c.IsEmpty() {
		switch network {
		case "udp", "udp4", "udp6":
			if address == "" {
				return net.ListenUDP(network, nil)
			}
		default:
		}
		d := &net.Dialer{
			Timeout: util.DialTimeout,
			// LocalAddr: laddr, // TODO: optional local address
		}
		return d.DialContext(ctx, network, ipAddr)
	}

	conn, err := c.getConn(ctx)
	if err != nil {
		return nil, err
	}

	cc, err := c.Node().Client.ConnectContext(ctx, conn, network, ipAddr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return cc, nil
}

func (*Chain) resolve(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
			return fmt.Sprintf("%s:%s", ips[0].String(), port)
		}
	}
	return addr
}

// Conn obtains a handshaked connection to the last node of the chain.
func (c *Chain) Conn() (conn net.Conn, err error) {
	ctx := context.Background()

	retries := 1
	if c != nil && c.Retries > 0 {
		retries = c.Retries
	}

	for i := 0; i < retries; i++ {
		conn, err = c.getConn(ctx)
		if err == nil {
			break
		}
	}
	return
}

// getConn obtains a connection to the last node of the chain.
func (c *Chain) getConn(_ context.Context) (conn net.Conn, err error) {
	if c.IsEmpty() {
		err = ErrEmptyChain
		return
	}
	cc, err := c.Node().Client.Dial(c.Node().Addr)
	if err != nil {
		return
	}

	conn = cc
	return
}
