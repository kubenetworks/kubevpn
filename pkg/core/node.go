package core

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

type Node struct {
	Addr     string
	Protocol string
	Remote   string // remote address, used by tcp/udp port forwarding
	Values   url.Values
	Client   *Client
}

// ParseNode pattern is [scheme://][user:pass@host]:port.
func ParseNode(s string) (*Node, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return nil, errors.New("invalid node")
	}
	u, err := url.Parse(s)
	if err != nil {
		err = errors.Wrap(err, "url.Parse(s): ")
		return nil, err
	}
	return &Node{
		Addr:     u.Host,
		Remote:   strings.Trim(u.EscapedPath(), "/"),
		Values:   u.Query(),
		Protocol: u.Scheme,
	}, nil
}

// Get returns node parameter specified by key.
func (node *Node) Get(key string) string {
	values := node.Values[key]
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return node.Values.Get(key)
}

// GetInt converts node parameter value to int.
func (node *Node) GetInt(key string) int {
	n, _ := strconv.Atoi(node.Get(key))
	return n
}
