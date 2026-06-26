package core

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var ErrorInvalidNode = errors.New("invalid node")

// Node is a parsed listener/forwarder URI — pure data, no transport state.
//
// URI schema:
//
//	protocol://[host:port][/forward-uri][?key=value&...]
//
// Supported protocols and their parameters:
//
//	tun     TUN device listener/client
//	          Addr:    optional listen address
//	          Forward: optional remote server URI (makes this a client)
//	          Params:  net (CIDR), net6 (CIDR), mtu (int), name (string),
//	                   route (comma-separated CIDRs), gw (gateway IP)
//
//	gtcp    Gvisor TCP listener
//	          Addr:    required (e.g. ":10801")
//
//	gudp    Gvisor UDP over TCP listener
//	          Addr:    required
//
//	ssh     SSH tunnel listener
//	          Addr:    required
//
// Examples:
//
//	NewNode("gtcp", ":10801")                                         → "gtcp://:10801"
//	NewNode("tun", "").WithParam("net", "198.18.0.100/16")            → "tun://?net=198.18.0.100/16"
//	NewNode("tun", "").WithForward("tcp://10.0.0.1:8422")             → "tun:/tcp://10.0.0.1:8422"
//	    .WithParam("net", "198.18.0.102/16").WithParam("route", "…")
type Node struct {
	Protocol string     // scheme identifying the protocol
	Addr     string     // host:port to listen on
	Forward  string     // forwarding target sub-URI (from URL path), empty if none
	Values   url.Values // query parameters (protocol-specific, see schema above)
}

// NewNode creates a Node with the given protocol and listen address.
func NewNode(protocol, addr string) *Node {
	return &Node{
		Protocol: protocol,
		Addr:     addr,
		Values:   make(url.Values),
	}
}

// WithForward sets the forwarding target URI.
func (node *Node) WithForward(forward string) *Node {
	node.Forward = forward
	return node
}

// WithParam sets a query parameter.
func (node *Node) WithParam(key, value string) *Node {
	node.Values.Set(key, value)
	return node
}

func (node *Node) String() string {
	var b strings.Builder
	b.WriteString(node.Protocol)
	b.WriteString("://")
	b.WriteString(node.Addr)
	if node.Forward != "" {
		if !strings.HasPrefix(node.Forward, "/") {
			b.WriteByte('/')
		}
		b.WriteString(node.Forward)
	}
	if len(node.Values) > 0 {
		b.WriteByte('?')
		b.WriteString(node.Values.Encode())
	}
	return b.String()
}

// ParseNode parses a listener/forwarder URI string into a Node.
func ParseNode(s string) (*Node, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrorInvalidNode
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" {
		return nil, fmt.Errorf("%w: missing protocol scheme in %q", ErrorInvalidNode, s)
	}
	return &Node{
		Protocol: u.Scheme,
		Addr:     u.Host,
		Forward:  strings.Trim(u.EscapedPath(), "/"),
		Values:   u.Query(),
	}, nil
}

// Get returns the first non-empty value for key, or "".
func (node *Node) Get(key string) string {
	for _, v := range node.Values[key] {
		if v != "" {
			return v
		}
	}
	return ""
}

// GetInt returns the value for key as an int, or 0 if absent/unparseable.
func (node *Node) GetInt(key string) int {
	n, _ := strconv.Atoi(node.Get(key))
	return n
}
