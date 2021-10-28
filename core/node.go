package core

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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
		url:    u,
	}

	u.RawQuery = ""
	u.User = nil

	switch u.Scheme {
	case "tun":
		node.Protocol = u.Scheme
		node.Transport = u.Scheme
	case "tcp":
		node.Protocol = "tcp"
		node.Transport = "tcp"
	default:
		return Node{}, ErrInvalidNode
	}
	return
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
