package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"github.com/ginuerzh/gost"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-log/log"
)

type stringList []string

func (l *stringList) String() string {
	return fmt.Sprintf("%s", *l)
}
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

type route struct {
	ServeNodes stringList
	ChainNodes stringList
	Retries    int
}

func (r *route) parseChain() (*gost.Chain, error) {
	chain := gost.NewChain()
	chain.Retries = r.Retries
	gid := 1 // group ID

	for _, ns := range r.ChainNodes {
		ngroup := gost.NewNodeGroup()
		ngroup.ID = gid
		gid++

		// parse the base nodes
		nodes, err := parseChainNode(ns)
		if err != nil {
			return nil, err
		}

		nid := 1 // node ID
		for i := range nodes {
			nodes[i].ID = nid
			nid++
		}
		ngroup.AddNode(nodes...)

		ngroup.SetSelector(nil,
			gost.WithFilter(
				&gost.FailFilter{
					MaxFails:    nodes[0].GetInt("max_fails"),
					FailTimeout: nodes[0].GetDuration("fail_timeout"),
				},
				&gost.InvalidFilter{},
			),
			gost.WithStrategy(gost.NewStrategy(nodes[0].Get("strategy"))),
		)

		if cfg := nodes[0].Get("peer"); cfg != "" {
			f, err := os.Open(cfg)
			if err != nil {
				return nil, err
			}

			peerCfg := newPeerConfig()
			peerCfg.group = ngroup
			peerCfg.baseNodes = nodes
			peerCfg.Reload(f)
			f.Close()

			go gost.PeriodReload(peerCfg, cfg)
		}

		chain.AddNodeGroup(ngroup)
	}

	return chain, nil
}

func parseChainNode(ns string) (nodes []gost.Node, err error) {
	node, err := gost.ParseNode(ns)
	if err != nil {
		return
	}

	if auth := node.Get("auth"); auth != "" && node.User == nil {
		c, err := base64.StdEncoding.DecodeString(auth)
		if err != nil {
			return nil, err
		}
		cs := string(c)
		s := strings.IndexByte(cs, ':')
		if s < 0 {
			node.User = url.User(cs)
		} else {
			node.User = url.UserPassword(cs[:s], cs[s+1:])
		}
	}
	if node.User == nil {
		users, err := parseUsers(node.Get("secrets"))
		if err != nil {
			return nil, err
		}
		if len(users) > 0 {
			node.User = users[0]
		}
	}

	serverName, sport, _ := net.SplitHostPort(node.Addr)
	if serverName == "" {
		serverName = "localhost" // default server name
	}

	rootCAs, err := loadCA(node.Get("ca"))
	if err != nil {
		return
	}
	tlsCfg := &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: !node.GetBool("secure"),
		RootCAs:            rootCAs,
	}

	// If the argument `ca` is given, but not open `secure`, we verify the
	// certificate manually.
	if rootCAs != nil && !node.GetBool("secure") {
		tlsCfg.VerifyConnection = func(state tls.ConnectionState) error {
			opts := x509.VerifyOptions{
				Roots:         rootCAs,
				CurrentTime:   time.Now(),
				DNSName:       "",
				Intermediates: x509.NewCertPool(),
			}

			certs := state.PeerCertificates
			for i, cert := range certs {
				if i == 0 {
					continue
				}
				opts.Intermediates.AddCert(cert)
			}

			_, err = certs[0].Verify(opts)
			return err
		}
	}

	if cert, err := tls.LoadX509KeyPair(node.Get("cert"), node.Get("key")); err == nil {
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	timeout := node.GetDuration("timeout")

	var tr gost.Transporter
	switch node.Transport {
	case "ssh":
		if node.Protocol == "direct" || node.Protocol == "remote" {
			tr = gost.SSHForwardTransporter()
		} else {
			tr = gost.SSHTunnelTransporter()
		}
	default:
		tr = gost.TCPTransporter()
	}

	var connector gost.Connector
	switch node.Protocol {
	case "ssu":
		connector = gost.ShadowUDPConnector(node.User)
	case "direct":
		connector = gost.SSHDirectForwardConnector()
	case "remote":
		connector = gost.SSHRemoteForwardConnector()
	default:
		connector = gost.AutoConnector(node.User)
	}

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

	sshConfig := &gost.SSHConfig{}
	if s := node.Get("ssh_key"); s != "" {
		key, err := gost.ParseSSHKeyFile(s)
		if err != nil {
			return nil, err
		}
		sshConfig.Key = key
	}
	handshakeOptions := []gost.HandshakeOption{
		gost.AddrHandshakeOption(node.Addr),
		gost.HostHandshakeOption(host),
		gost.UserHandshakeOption(node.User),
		gost.TLSConfigHandshakeOption(tlsCfg),
		gost.IntervalHandshakeOption(node.GetDuration("ping")),
		gost.TimeoutHandshakeOption(timeout),
		gost.RetryHandshakeOption(node.GetInt("retry")),
		gost.SSHConfigHandshakeOption(sshConfig),
	}

	node.Client = &gost.Client{
		Connector:   connector,
		Transporter: tr,
	}

	node.Bypass = parseBypass(node.Get("bypass"))

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
		return nil, err
	}

	var rts []router

	for _, ns := range r.ServeNodes {
		node, err := gost.ParseNode(ns)
		if err != nil {
			return nil, err
		}

		if auth := node.Get("auth"); auth != "" && node.User == nil {
			c, err := base64.StdEncoding.DecodeString(auth)
			if err != nil {
				return nil, err
			}
			cs := string(c)
			s := strings.IndexByte(cs, ':')
			if s < 0 {
				node.User = url.User(cs)
			} else {
				node.User = url.UserPassword(cs[:s], cs[s+1:])
			}
		}
		authenticator, err := parseAuthenticator(node.Get("secrets"))
		if err != nil {
			return nil, err
		}
		if authenticator == nil && node.User != nil {
			kvs := make(map[string]string)
			kvs[node.User.Username()], _ = node.User.Password()
			authenticator = gost.NewLocalAuthenticator(kvs)
		}
		if node.User == nil {
			if users, _ := parseUsers(node.Get("secrets")); len(users) > 0 {
				node.User = users[0]
			}
		}
		certFile, keyFile := node.Get("cert"), node.Get("key")
		tlsCfg, err := tlsConfig(certFile, keyFile, node.Get("ca"))
		if err != nil && certFile != "" && keyFile != "" {
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
		case "ssh":
			config := &gost.SSHConfig{
				Authenticator: authenticator,
				TLSConfig:     tlsCfg,
			}
			if s := node.Get("ssh_key"); s != "" {
				key, err := gost.ParseSSHKeyFile(s)
				if err != nil {
					return nil, err
				}
				config.Key = key
			}
			if s := node.Get("ssh_authorized_keys"); s != "" {
				keys, err := gost.ParseSSHAuthorizedKeysFile(s)
				if err != nil {
					return nil, err
				}
				config.AuthorizedKeys = keys
			}
			if node.Protocol == "forward" {
				ln, err = gost.TCPListener(node.Addr)
			} else {
				ln, err = gost.SSHTunnelListener(node.Addr, config)
			}
		case "tcp":
			// Directly use SSH port forwarding if the last chain node is forward+ssh
			if chain.LastNode().Protocol == "forward" && chain.LastNode().Transport == "ssh" {
				chain.Nodes()[len(chain.Nodes())-1].Client.Connector = gost.SSHDirectForwardConnector()
				chain.Nodes()[len(chain.Nodes())-1].Client.Transporter = gost.SSHForwardTransporter()
			}
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
		case "dns":
			ln, err = gost.DNSListener(
				node.Addr,
				&gost.DNSOptions{
					Mode:      node.Get("mode"),
					TLSConfig: tlsCfg,
				},
			)
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
		case "dns":
			handler = gost.DNSHandler(node.Remote)
		default:
			// start from 2.5, if remote is not empty, then we assume that it is a forward tunnel.
			if node.Remote != "" {
				handler = gost.TCPDirectForwardHandler(node.Remote)
			} else {
				handler = gost.AutoHandler()
			}
		}

		node.Bypass = parseBypass(node.Get("bypass"))
		hosts := parseHosts(node.Get("hosts"))
		ips := parseIP(node.Get("ip"), "")

		resolver := parseResolver(node.Get("dns"))
		if resolver != nil {
			resolver.Init(
				gost.ChainResolverOption(chain),
				gost.TimeoutResolverOption(timeout),
				gost.TTLResolverOption(ttl),
				gost.PreferResolverOption(node.Get("prefer")),
				gost.SrcIPResolverOption(net.ParseIP(node.Get("ip"))),
			)
		}

		handler.Init(
			gost.AddrHandlerOption(ln.Addr().String()),
			gost.ChainHandlerOption(chain),
			gost.UsersHandlerOption(node.User),
			gost.AuthenticatorHandlerOption(authenticator),
			gost.BypassHandlerOption(node.Bypass),
			gost.ResolverHandlerOption(resolver),
			gost.HostsHandlerOption(hosts),
			gost.RetryHandlerOption(node.GetInt("retry")), // override the global retry option.
			gost.TimeoutHandlerOption(timeout),
			gost.ProbeResistHandlerOption(node.Get("probe_resist")),
			gost.KnockingHandlerOption(node.Get("knock")),
			gost.NodeHandlerOption(node),
			gost.IPsHandlerOption(ips),
			gost.TCPModeHandlerOption(node.GetBool("tcp")),
			gost.IPRoutesHandlerOption(tunRoutes...),
		)

		rt := router{
			node:     node,
			server:   &gost.Server{Listener: ln},
			handler:  handler,
			chain:    chain,
			resolver: resolver,
			hosts:    hosts,
		}
		rts = append(rts, rt)
	}

	return rts, nil
}

type router struct {
	node     gost.Node
	server   *gost.Server
	handler  gost.Handler
	chain    *gost.Chain
	resolver gost.Resolver
	hosts    *gost.Hosts
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
