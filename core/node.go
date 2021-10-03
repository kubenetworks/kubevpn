package core

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrInvalidNode is an error that implies the node is invalid.
	ErrInvalidNode = errors.New("invalid node")
)

// Node is a proxy node, mainly used to construct a proxy chain.
type Node struct {
	Addr      string
	Protocol  string
	Transport string
	Remote    string   // remote address, used by tcp/udp port forwarding
	url       *url.URL // raw url
	User      *url.Userinfo
	Values    url.Values
	Client    *Client
}

// ParseNode parses the node info.
// The proxy node string pattern is [scheme://][user:pass@host]:port.
func ParseNode(s string) (node Node, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Node{}, ErrInvalidNode
	}
	u, err := url.Parse(s)
	if err != nil {
		return
	}

	node = Node{
		Addr:   u.Host,
		Remote: strings.Trim(u.EscapedPath(), "/"),
		Values: u.Query(),
		User:   u.User,
		url:    u,
	}

	u.RawQuery = ""
	u.User = nil

	switch u.Scheme {
	case "tun":
		node.Protocol = u.Scheme
		node.Transport = u.Scheme
	case "socks5":
		node.Protocol = u.Scheme
		node.Transport = "tcp"
	default:
		return Node{}, ErrInvalidNode
	}
	return
}

// Clone clones the node, it will prevent data race.
func (node *Node) Clone() Node {
	nd := *node
	return nd
}

// Get returns node parameter specified by key.
func (node *Node) Get(key string) string {
	return node.Values.Get(key)
}

// GetBool converts node parameter value to bool.
func (node *Node) GetBool(key string) bool {
	b, _ := strconv.ParseBool(node.Values.Get(key))
	return b
}

// GetInt converts node parameter value to int.
func (node *Node) GetInt(key string) int {
	n, _ := strconv.Atoi(node.Get(key))
	return n
}

// GetDuration converts node parameter value to time.Duration.
func (node *Node) GetDuration(key string) time.Duration {
	d, err := time.ParseDuration(node.Get(key))
	if err != nil {
		d = time.Duration(node.GetInt(key)) * time.Second
	}
	return d
}

func (node Node) String() string {
	var scheme string
	if node.url != nil {
		scheme = node.url.Scheme
	}
	if scheme == "" {
		scheme = fmt.Sprintf("%s+%s", node.Protocol, node.Transport)
	}
	return fmt.Sprintf("%s://%s", scheme, node.Addr)
}

// NodeGroup is a group of nodes.
type NodeGroup struct {
	ID    int
	nodes []Node
	mux   sync.RWMutex
}

// NewNodeGroup creates a node group
func NewNodeGroup(nodes ...Node) *NodeGroup {
	return &NodeGroup{
		nodes: nodes,
	}
}

// AddNode appends node or node list into group node.
func (group *NodeGroup) AddNode(node ...Node) {
	if group == nil {
		return
	}
	group.mux.Lock()
	defer group.mux.Unlock()

	group.nodes = append(group.nodes, node...)
}

// SetNodes replaces the group nodes to the specified nodes,
// and returns the previous nodes.
func (group *NodeGroup) SetNodes(nodes ...Node) []Node {
	if group == nil {
		return nil
	}

	group.mux.Lock()
	defer group.mux.Unlock()

	old := group.nodes
	group.nodes = nodes
	return old
}

// Nodes returns the node list in the group
func (group *NodeGroup) Nodes() []Node {
	if group == nil {
		return nil
	}

	group.mux.RLock()
	defer group.mux.RUnlock()

	return group.nodes
}

// GetNode returns the node specified by index in the group.
func (group *NodeGroup) GetNode(i int) Node {
	group.mux.RLock()
	defer group.mux.RUnlock()

	if i < 0 || group == nil || len(group.nodes) <= i {
		return Node{}
	}
	return group.nodes[i]
}

// Next selects a node from group.
// It also selects IP if the IP list exists.
func (group *NodeGroup) Next() (node Node, err error) {
	if group == nil {
		return
	}
	node = group.nodes[0]
	return
}
