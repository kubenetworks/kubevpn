package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	gost2 "kubevpn/gost"

	//"gost"
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

func (r *route) parseChain() (*gost2.Chain, error) {
	chain := gost2.NewChain()
	chain.Retries = r.Retries
	gid := 1 // group ID

	for _, ns := range r.ChainNodes {
		ngroup := gost2.NewNodeGroup()
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
			gost2.WithFilter(
				&gost2.FailFilter{
					MaxFails:    nodes[0].GetInt("max_fails"),
					FailTimeout: nodes[0].GetDuration("fail_timeout"),
				},
				&gost2.InvalidFilter{},
			),
			gost2.WithStrategy(gost2.NewStrategy(nodes[0].Get("strategy"))),
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

			go gost2.PeriodReload(peerCfg, cfg)
		}

		chain.AddNodeGroup(ngroup)
	}

	return chain, nil
}

func parseChainNode(ns string) (nodes []gost2.Node, err error) {
	node, err := gost2.ParseNode(ns)
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

	var tr gost2.Transporter
	switch node.Transport {
	case "ssh":
		if node.Protocol == "direct" || node.Protocol == "remote" {
			tr = gost2.SSHForwardTransporter()
		} else {
			tr = gost2.SSHTunnelTransporter()
		}
	default:
		tr = gost2.TCPTransporter()
	}

	var connector gost2.Connector
	switch node.Protocol {
	case "ssu":
		connector = gost2.ShadowUDPConnector(node.User)
	case "direct":
		connector = gost2.SSHDirectForwardConnector()
	case "remote":
		connector = gost2.SSHRemoteForwardConnector()
	default:
		connector = gost2.AutoConnector(node.User)
	}

	host := node.Get("host")
	if host == "" {
		host = node.Host
	}

	node.DialOptions = append(node.DialOptions,
		gost2.TimeoutDialOption(timeout),
		gost2.HostDialOption(host),
	)

	node.ConnectOptions = []gost2.ConnectOption{
		gost2.UserAgentConnectOption(node.Get("agent")),
		gost2.NoTLSConnectOption(node.GetBool("notls")),
		gost2.NoDelayConnectOption(node.GetBool("nodelay")),
	}

	sshConfig := &gost2.SSHConfig{}
	if s := node.Get("ssh_key"); s != "" {
		key, err := gost2.ParseSSHKeyFile(s)
		if err != nil {
			return nil, err
		}
		sshConfig.Key = key
	}
	handshakeOptions := []gost2.HandshakeOption{
		gost2.AddrHandshakeOption(node.Addr),
		gost2.HostHandshakeOption(host),
		gost2.UserHandshakeOption(node.User),
		gost2.TLSConfigHandshakeOption(tlsCfg),
		gost2.IntervalHandshakeOption(node.GetDuration("ping")),
		gost2.TimeoutHandshakeOption(timeout),
		gost2.RetryHandshakeOption(node.GetInt("retry")),
		gost2.SSHConfigHandshakeOption(sshConfig),
	}

	node.Client = &gost2.Client{
		Connector:   connector,
		Transporter: tr,
	}

	node.Bypass = parseBypass(node.Get("bypass"))

	ips := parseIP(node.Get("ip"), sport)
	for _, ip := range ips {
		nd := node.Clone()
		nd.Addr = ip
		// override the default node address
		nd.HandshakeOptions = append(handshakeOptions, gost2.AddrHandshakeOption(ip))
		// One node per IP
		nodes = append(nodes, nd)
	}
	if len(ips) == 0 {
		node.HandshakeOptions = handshakeOptions
		nodes = []gost2.Node{node}
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
		node, err := gost2.ParseNode(ns)
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
			authenticator = gost2.NewLocalAuthenticator(kvs)
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

		var ln gost2.Listener
		switch node.Transport {
		case "ssh":
			config := &gost2.SSHConfig{
				Authenticator: authenticator,
				TLSConfig:     tlsCfg,
			}
			if s := node.Get("ssh_key"); s != "" {
				key, err := gost2.ParseSSHKeyFile(s)
				if err != nil {
					return nil, err
				}
				config.Key = key
			}
			if s := node.Get("ssh_authorized_keys"); s != "" {
				keys, err := gost2.ParseSSHAuthorizedKeysFile(s)
				if err != nil {
					return nil, err
				}
				config.AuthorizedKeys = keys
			}
			if node.Protocol == "forward" {
				ln, err = gost2.TCPListener(node.Addr)
			} else {
				ln, err = gost2.SSHTunnelListener(node.Addr, config)
			}
		case "tcp":
			// Directly use SSH port forwarding if the last chain node is forward+ssh
			if chain.LastNode().Protocol == "forward" && chain.LastNode().Transport == "ssh" {
				chain.Nodes()[len(chain.Nodes())-1].Client.Connector = gost2.SSHDirectForwardConnector()
				chain.Nodes()[len(chain.Nodes())-1].Client.Transporter = gost2.SSHForwardTransporter()
			}
			ln, err = gost2.TCPListener(node.Addr)
		case "udp":
			ln, err = gost2.UDPListener(node.Addr, &gost2.UDPListenConfig{
				TTL:       ttl,
				Backlog:   node.GetInt("backlog"),
				QueueSize: node.GetInt("queue"),
			})
		case "tun":
			cfg := gost2.TunConfig{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				Peer:    node.Get("peer"),
				MTU:     node.GetInt("mtu"),
				Routes:  tunRoutes,
				Gateway: node.Get("gw"),
			}
			ln, err = gost2.TunListener(cfg)
		case "tap":
			cfg := gost2.TapConfig{
				Name:    node.Get("name"),
				Addr:    node.Get("net"),
				MTU:     node.GetInt("mtu"),
				Routes:  strings.Split(node.Get("route"), ","),
				Gateway: node.Get("gw"),
			}
			ln, err = gost2.TapListener(cfg)
		case "dns":
			ln, err = gost2.DNSListener(
				node.Addr,
				&gost2.DNSOptions{
					Mode:      node.Get("mode"),
					TLSConfig: tlsCfg,
				},
			)
		default:
			ln, err = gost2.TCPListener(node.Addr)
		}
		if err != nil {
			return nil, err
		}

		var handler gost2.Handler
		switch node.Protocol {
		case "tcp":
			handler = gost2.TCPDirectForwardHandler(node.Remote)
		case "udp":
			handler = gost2.UDPDirectForwardHandler(node.Remote)
		case "tun":
			handler = gost2.TunHandler()
		case "tap":
			handler = gost2.TapHandler()
		case "dns":
			handler = gost2.DNSHandler(node.Remote)
		default:
			// start from 2.5, if remote is not empty, then we assume that it is a forward tunnel.
			if node.Remote != "" {
				handler = gost2.TCPDirectForwardHandler(node.Remote)
			} else {
				handler = gost2.AutoHandler()
			}
		}

		node.Bypass = parseBypass(node.Get("bypass"))
		hosts := parseHosts(node.Get("hosts"))
		ips := parseIP(node.Get("ip"), "")

		resolver := parseResolver(node.Get("dns"))
		if resolver != nil {
			resolver.Init(
				gost2.ChainResolverOption(chain),
				gost2.TimeoutResolverOption(timeout),
				gost2.TTLResolverOption(ttl),
				gost2.PreferResolverOption(node.Get("prefer")),
				gost2.SrcIPResolverOption(net.ParseIP(node.Get("ip"))),
			)
		}

		handler.Init(
			gost2.AddrHandlerOption(ln.Addr().String()),
			gost2.ChainHandlerOption(chain),
			gost2.UsersHandlerOption(node.User),
			gost2.AuthenticatorHandlerOption(authenticator),
			gost2.BypassHandlerOption(node.Bypass),
			gost2.ResolverHandlerOption(resolver),
			gost2.HostsHandlerOption(hosts),
			gost2.RetryHandlerOption(node.GetInt("retry")), // override the global retry option.
			gost2.TimeoutHandlerOption(timeout),
			gost2.ProbeResistHandlerOption(node.Get("probe_resist")),
			gost2.KnockingHandlerOption(node.Get("knock")),
			gost2.NodeHandlerOption(node),
			gost2.IPsHandlerOption(ips),
			gost2.TCPModeHandlerOption(node.GetBool("tcp")),
			gost2.IPRoutesHandlerOption(tunRoutes...),
		)

		rt := router{
			node:     node,
			server:   &gost2.Server{Listener: ln},
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
	node     gost2.Node
	server   *gost2.Server
	handler  gost2.Handler
	chain    *gost2.Chain
	resolver gost2.Resolver
	hosts    *gost2.Hosts
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
