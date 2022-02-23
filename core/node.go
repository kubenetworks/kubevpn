package core

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

var (
	ErrorInvalidNode = errors.New("invalid node")
)

type Node struct {
	Addr      string
	Protocol  string
	Transport string
	Remote    string // remote address, used by tcp/udp port forwarding
	Values    url.Values
	Client    *Client
}

// ParseNode parses the node info.
// The proxy node string pattern is [scheme://][user:pass@host]:port.
func ParseNode(s string) (node *Node, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrorInvalidNode
	}
	u, err := url.Parse(s)
	if err != nil {
		return
	}

	node = &Node{
		Addr:   u.Host,
		Remote: strings.Trim(u.EscapedPath(), "/"),
		Values: u.Query(),
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
		return nil, ErrorInvalidNode
	}
	return
}

// Get returns node parameter specified by key.
func (node *Node) Get(key string) string {
	return node.Values.Get(key)
}

// GetInt converts node parameter value to int.
func (node *Node) GetInt(key string) int {
	n, _ := strconv.Atoi(node.Get(key))
	return n
}
