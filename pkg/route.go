package main

import (
	"github.com/go-log/log"
	"github.com/pkg/errors"
	"kubevpn/gost"
	"net"
	"strings"
)

type route struct {
	ServeNodes []string // tun
	ChainNodes string   // socks5
	Retries    int
}

func (r *route) parseChain() (*gost.Chain, error) {
	chain := gost.NewChain()
	chain.Retries = r.Retries
	gid := 1 // group ID

	ngroup := gost.NewNodeGroup()
	ngroup.ID = gid
	gid++

	// parse the base nodes
	nodes, err := parseChainNode(r.ChainNodes)
	if err != nil {
		return nil, err
	}

	nid := 1 // node ID
	for i := range nodes {
		nodes[i].ID = nid
		nid++
	}
	ngroup.AddNode(nodes...)

	chain.AddNodeGroup(ngroup)

	return chain, nil
}

func parseChainNode(ns string) (nodes []gost.Node, err error) {
	node, err := gost.ParseNode(ns)
	if err != nil {
		return
	}
	serverName, sport, _ := net.SplitHostPort(node.Addr)
	if serverName == "" {
		serverName = "localhost" // default server name
	}
	timeout := node.GetDuration("timeout")

	host := node.Get("host")
	if host == "" {
		host = node.Host
	}

	node.DialOptions = append(node.DialOptions,
		gost.TimeoutDialOption(timeout),
		gost.HostDialOption(host),
	)

	node.ConnectOptions = []gost.ConnectOption{
		gost.UserAgentConnectOption(node.Get("agent")),
		gost.NoTLSConnectOption(node.GetBool("notls")),
		gost.NoDelayConnectOption(node.GetBool("nodelay")),
	}

	handshakeOptions := []gost.HandshakeOption{
		gost.AddrHandshakeOption(node.Addr),
		gost.HostHandshakeOption(host),
		gost.IntervalHandshakeOption(node.GetDuration("ping")),
		gost.TimeoutHandshakeOption(timeout),
		gost.RetryHandshakeOption(node.GetInt("retry")),
	}

	node.Client = &gost.Client{
		Connector:   gost.AutoConnector(node.User),
		Transporter: gost.TCPTransporter(),
	}

	ips := parseIP(node.Get("ip"), sport)
	for _, ip := range ips {
		nd := node.Clone()
		nd.Addr = ip
		// override the default node address
		nd.HandshakeOptions = append(handshakeOptions, gost.AddrHandshakeOption(ip))
		// One node per IP
		nodes = append(nodes, nd)
	}
	if len(ips) == 0 {
		node.HandshakeOptions = handshakeOptions
		nodes = []gost.Node{node}
	}

	return
}

func (r *route) GenRouters() ([]router, error) {
	chain, err := r.parseChain()
	if err != nil {
		if !errors.Is(err, gost.ErrInvalidNode) {
			return nil, err
		}
	}

	routers := make([]router, 0, len(r.ServeNodes))
	for _, serveNode := range r.ServeNodes {
		node, err := gost.ParseNode(serveNode)
		if err != nil {
			return nil, err
		}
		ttl := node.GetDuration("ttl")
		timeout := node.GetDuration("timeout")

		tunRoutes := parseIPRoutes(node.Get("route"))
		gw := net.ParseIP(node.Get("gw")) // default gateway
		for i := range tunRoutes {
			if tunRoutes[i].Gateway == nil {
				tunRoutes[i].Gateway = gw
			}
		}

		var ln gost.Listener
		switch node.Transport {
		case "tcp":
			// Directly use SSH port forwarding if the last chain node is forward+ssh
			ln, err = gost.TCPListener(node.Addr)
		case "udp":
			ln, err = gost.UDPListener(node.Addr, &gost.UDPListenConfig{
				TTL:       ttl,
				Backlog:   node.GetInt("backlog"),
				QueueSize: node.GetInt("queue"),
			})
		case "tun":
			cfg := gost.TunConfig{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				Peer:    node.Get("peer"),
				MTU:     node.GetInt("mtu"),
				Routes:  tunRoutes,
				Gateway: node.Get("gw"),
			}
			ln, err = gost.TunListener(cfg)
		case "tap":
			cfg := gost.TapConfig{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				MTU:     node.GetInt("mtu"),
				Routes:  strings.Split(node.Get("route"), ","),
				Gateway: node.Get("gw"),
			}
			ln, err = gost.TapListener(cfg)
		default:
			ln, err = gost.TCPListener(node.Addr)
		}
		if err != nil {
			return nil, err
		}

		var handler gost.Handler
		switch node.Protocol {
		case "tcp":
			handler = gost.TCPDirectForwardHandler(node.Remote)
		case "udp":
			handler = gost.UDPDirectForwardHandler(node.Remote)
		case "tun":
			handler = gost.TunHandler()
		case "tap":
			handler = gost.TapHandler()
		default:
			// start from 2.5, if remote is not empty, then we assume that it is a forward tunnel.
			if node.Remote != "" {
				handler = gost.TCPDirectForwardHandler(node.Remote)
			} else {
				handler = gost.AutoHandler()
			}
		}

		handler.Init(
			gost.AddrHandlerOption(ln.Addr().String()),
			gost.ChainHandlerOption(chain),
			gost.RetryHandlerOption(node.GetInt("retry")), // override the global retry option.
			gost.TimeoutHandlerOption(timeout),
			gost.NodeHandlerOption(node),
			gost.TCPModeHandlerOption(node.GetBool("tcp")),
			gost.IPRoutesHandlerOption(tunRoutes...),
		)

		rt := router{
			node:    node,
			server:  &gost.Server{Listener: ln},
			handler: handler,
			chain:   chain,
		}
		routers = append(routers, rt)
	}

	return routers, nil
}

type router struct {
	node    gost.Node
	server  *gost.Server
	handler gost.Handler
	chain   *gost.Chain
}

func (r *router) Serve() error {
	log.Logf("%s on %s", r.node.String(), r.server.Addr())
	return r.server.Serve(r.handler)
}

func (r *router) Close() error {
	if r == nil || r.server == nil {
		return nil
	}
	return r.server.Close()
}
