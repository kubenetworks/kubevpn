package core

import (
	"context"
	"errors"
	"fmt"
	"kubevpn/util"
	"net"
)

var (
	// ErrEmptyChain is an error that implies the chain is empty.
	ErrEmptyChain = errors.New("empty chain")
)

// Chain is a proxy chain that holds a list of proxy node groups.
type Chain struct {
	isRoute    bool
	Retries    int
	nodeGroups []*NodeGroup
	route      []Node // nodes in the selected route
}

// NewChain creates a proxy chain with a list of proxy nodes.
// It creates the node groups automatically, one group per node.
func NewChain(nodes ...Node) *Chain {
	chain := &Chain{}
	for _, node := range nodes {
		chain.nodeGroups = append(chain.nodeGroups, NewNodeGroup(node))
	}
	return chain
}

// newRoute creates a chain route.
// a chain route is the final route after node selection.
func newRoute(nodes ...Node) *Chain {
	chain := NewChain(nodes...)
	chain.isRoute = true
	return chain
}

// Nodes returns the proxy nodes that the chain holds.
// The first node in each group will be returned.
func (c *Chain) Nodes() (nodes []Node) {
	for _, group := range c.nodeGroups {
		if ns := group.Nodes(); len(ns) > 0 {
			nodes = append(nodes, ns[0])
		}
	}
	return
}

// NodeGroups returns the list of node group.
func (c *Chain) NodeGroups() []*NodeGroup {
	return c.nodeGroups
}

// LastNode returns the last node of the node list.
// If the chain is empty, an empty node will be returned.
// If the last node is a node group, the first node in the group will be returned.
func (c *Chain) LastNode() Node {
	if c.IsEmpty() {
		return Node{}
	}
	group := c.nodeGroups[len(c.nodeGroups)-1]
	return group.GetNode(0)
}

// LastNodeGroup returns the last group of the group list.
func (c *Chain) LastNodeGroup() *NodeGroup {
	if c.IsEmpty() {
		return nil
	}
	return c.nodeGroups[len(c.nodeGroups)-1]
}

// AddNode appends the node(s) to the chain.
func (c *Chain) AddNode(nodes ...Node) {
	if c == nil {
		return
	}
	for _, node := range nodes {
		c.nodeGroups = append(c.nodeGroups, NewNodeGroup(node))
	}
}

// AddNodeGroup appends the group(s) to the chain.
func (c *Chain) AddNodeGroup(groups ...*NodeGroup) {
	if c == nil {
		return
	}
	for _, group := range groups {
		c.nodeGroups = append(c.nodeGroups, group)
	}
}

// IsEmpty checks if the chain is empty.
// An empty chain means that there is no proxy node or node group in the chain.
func (c *Chain) IsEmpty() bool {
	return c == nil || len(c.nodeGroups) == 0
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
	route, err := c.selectRouteFor(address)
	if err != nil {
		return nil, err
	}

	ipAddr := address
	if address != "" {
		ipAddr = c.resolve(address)
	}

	if route.IsEmpty() {
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

	conn, err := route.getConn(ctx)
	if err != nil {
		return nil, err
	}

	cc, err := route.LastNode().Client.ConnectContext(ctx, conn, network, ipAddr)
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
		var route *Chain
		route, err = c.selectRoute()
		if err != nil {
			continue
		}
		conn, err = route.getConn(ctx)
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
	nodes := c.Nodes()
	node := nodes[0]

	cc, err := node.Client.Dial(node.Addr)
	if err != nil {
		return
	}

	conn = cc
	return
}

func (c *Chain) selectRoute() (route *Chain, err error) {
	return c.selectRouteFor("")
}

// selectRouteFor selects route with bypass testing.
func (c *Chain) selectRouteFor(addr string) (route *Chain, err error) {
	if c.IsEmpty() {
		return newRoute(), nil
	}
	if c.isRoute {
		return c, nil
	}

	route = newRoute()
	var nodes []Node

	for _, group := range c.nodeGroups {
		var node Node
		node, err = group.Next()
		if err != nil {
			return
		}

		route.AddNode(node)
		nodes = append(nodes, node)
	}

	route.route = nodes
	return
}
