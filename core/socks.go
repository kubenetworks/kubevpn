package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/ginuerzh/gosocks5"
	"github.com/go-log/log"
)

var (
	_ net.PacketConn = (*socks5UDPTunnelConn)(nil)
)

type clientSelector struct {
	methods   []uint8
	User      *url.Userinfo
	TLSConfig *tls.Config
}

func (selector *clientSelector) Methods() []uint8 {
	if Debug {
		log.Log("[socks5] methods:", selector.methods)
	}
	return selector.methods
}

func (selector *clientSelector) AddMethod(methods ...uint8) {
	selector.methods = append(selector.methods, methods...)
}

func (selector *clientSelector) Select(methods ...uint8) (method uint8) {
	return
}

func (selector *clientSelector) OnSelected(method uint8, conn net.Conn) (net.Conn, error) {
	if Debug {
		log.Log("[socks5] method selected:", method)
	}
	switch method {
	case gosocks5.MethodUserPass:
		var username, password string
		if selector.User != nil {
			username = selector.User.Username()
			password, _ = selector.User.Password()
		}

		req := gosocks5.NewUserPassRequest(gosocks5.UserPassVer, username, password)
		if err := req.Write(conn); err != nil {
			log.Log("[socks5]", err)
			return nil, err
		}
		if Debug {
			log.Log("[socks5]", req)
		}
		resp, err := gosocks5.ReadUserPassResponse(conn)
		if err != nil {
			log.Log("[socks5]", err)
			return nil, err
		}
		if Debug {
			log.Log("[socks5]", resp)
		}
		if resp.Status != gosocks5.Succeeded {
			return nil, gosocks5.ErrAuthFailure
		}
	case gosocks5.MethodNoAcceptable:
		return nil, gosocks5.ErrBadMethod
	}

	return conn, nil
}

type serverSelector struct {
	methods []uint8
	// Users     []*url.Userinfo
	Authenticator Authenticator
	TLSConfig     *tls.Config
}

func (selector *serverSelector) Methods() []uint8 {
	return selector.methods
}

func (selector *serverSelector) AddMethod(methods ...uint8) {
	selector.methods = append(selector.methods, methods...)
}

func (selector *serverSelector) Select(methods ...uint8) (method uint8) {
	if Debug {
		log.Logf("[socks5] %d %d %v", gosocks5.Ver5, len(methods), methods)
	}
	method = gosocks5.MethodNoAuth
	// when Authenticator is set, auth is mandatory
	if selector.Authenticator != nil {
		if method == gosocks5.MethodNoAuth {
			method = gosocks5.MethodUserPass
		}
	}

	return
}

func (selector *serverSelector) OnSelected(method uint8, conn net.Conn) (net.Conn, error) {
	if Debug {
		log.Logf("[socks5] %d %d", gosocks5.Ver5, method)
	}
	switch method {
	case gosocks5.MethodUserPass:
		req, err := gosocks5.ReadUserPassRequest(conn)
		if err != nil {
			log.Logf("[socks5] %s - %s: %s", conn.RemoteAddr(), conn.LocalAddr(), err)
			return nil, err
		}
		if Debug {
			log.Logf("[socks5] %s - %s: %s", conn.RemoteAddr(), conn.LocalAddr(), req.String())
		}

		if selector.Authenticator != nil && !selector.Authenticator.Authenticate(req.Username, req.Password) {
			resp := gosocks5.NewUserPassResponse(gosocks5.UserPassVer, gosocks5.Failure)
			if err := resp.Write(conn); err != nil {
				log.Logf("[socks5] %s - %s: %s", conn.RemoteAddr(), conn.LocalAddr(), err)
				return nil, err
			}
			if Debug {
				log.Logf("[socks5] %s - %s: %s", conn.RemoteAddr(), conn.LocalAddr(), resp)
			}
			log.Logf("[socks5] %s - %s: proxy authentication required", conn.RemoteAddr(), conn.LocalAddr())
			return nil, gosocks5.ErrAuthFailure
		}

		resp := gosocks5.NewUserPassResponse(gosocks5.UserPassVer, gosocks5.Succeeded)
		if err := resp.Write(conn); err != nil {
			log.Logf("[socks5] %s - %s: %s", conn.RemoteAddr(), conn.LocalAddr(), err)
			return nil, err
		}
		if Debug {
			log.Logf("[socks5] %s - %s: %s", conn.RemoteAddr(), conn.LocalAddr(), resp)
		}
	case gosocks5.MethodNoAcceptable:
		return nil, gosocks5.ErrBadMethod
	}

	return conn, nil
}

type socks5Connector struct {
	User *url.Userinfo
}

// SOCKS5Connector creates a connector for SOCKS5 proxy client.
// It accepts an optional auth info for SOCKS5 Username/Password Authentication.
func SOCKS5Connector(user *url.Userinfo) Connector {
	return &socks5Connector{User: user}
}

func (c *socks5Connector) Connect(conn net.Conn, address string, options ...ConnectOption) (net.Conn, error) {
	return c.ConnectContext(context.Background(), conn, "tcp", address, options...)
}

func (c *socks5Connector) ConnectContext(ctx context.Context, conn net.Conn, network, address string, options ...ConnectOption) (net.Conn, error) {
	switch network {
	case "udp", "udp4", "udp6":
		cnr := &socks5UDPTunConnector{User: c.User}
		return cnr.ConnectContext(ctx, conn, network, address, options...)
	}

	opts := &ConnectOptions{}
	for _, option := range options {
		option(opts)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ConnectTimeout
	}

	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	user := opts.User
	if user == nil {
		user = c.User
	}
	cc, err := socks5Handshake(conn,
		selectorSocks5HandshakeOption(opts.Selector),
		userSocks5HandshakeOption(user),
		noTLSSocks5HandshakeOption(opts.NoTLS),
	)
	if err != nil {
		return nil, err
	}
	conn = cc

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	p, _ := strconv.Atoi(port)
	req := gosocks5.NewRequest(gosocks5.CmdConnect, &gosocks5.Addr{
		Type: gosocks5.AddrDomain,
		Host: host,
		Port: uint16(p),
	})
	if err := req.Write(conn); err != nil {
		return nil, err
	}

	if Debug {
		log.Log("[socks5]", req)
	}

	reply, err := gosocks5.ReadReply(conn)
	if err != nil {
		return nil, err
	}

	if Debug {
		log.Log("[socks5]", reply)
	}

	if reply.Rep != gosocks5.Succeeded {
		return nil, errors.New("Service unavailable")
	}

	return conn, nil
}

type socks5UDPConnector struct {
	User *url.Userinfo
}

// SOCKS5UDPConnector creates a connector for SOCKS5 UDP relay.
// It accepts an optional auth info for SOCKS5 Username/Password Authentication.
func SOCKS5UDPConnector(user *url.Userinfo) Connector {
	return &socks5UDPConnector{User: user}
}

func (c *socks5UDPConnector) Connect(conn net.Conn, address string, options ...ConnectOption) (net.Conn, error) {
	return c.ConnectContext(context.Background(), conn, "udp", address, options...)
}

func (c *socks5UDPConnector) ConnectContext(ctx context.Context, conn net.Conn, network, address string, options ...ConnectOption) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil, fmt.Errorf("%s unsupported", network)
	}

	opts := &ConnectOptions{}
	for _, option := range options {
		option(opts)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ConnectTimeout
	}

	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	user := opts.User
	if user == nil {
		user = c.User
	}
	cc, err := socks5Handshake(conn,
		selectorSocks5HandshakeOption(opts.Selector),
		userSocks5HandshakeOption(user),
		noTLSSocks5HandshakeOption(opts.NoTLS),
	)
	if err != nil {
		return nil, err
	}
	conn = cc

	taddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}

	req := gosocks5.NewRequest(gosocks5.CmdUdp, &gosocks5.Addr{
		Type: gosocks5.AddrIPv4,
	})

	if err := req.Write(conn); err != nil {
		return nil, err
	}

	if Debug {
		log.Log("[socks5] udp\n", req)
	}

	reply, err := gosocks5.ReadReply(conn)
	if err != nil {
		return nil, err
	}

	if Debug {
		log.Log("[socks5] udp\n", reply)
	}

	if reply.Rep != gosocks5.Succeeded {
		log.Logf("[socks5] udp relay failure")
		return nil, fmt.Errorf("SOCKS5 udp relay failure")
	}
	baddr, err := net.ResolveUDPAddr("udp", reply.Addr.String())
	if err != nil {
		return nil, err
	}
	log.Logf("[socks5] udp associate on %s OK", baddr)

	uc, err := net.DialUDP("udp", nil, baddr)
	if err != nil {
		return nil, err
	}
	// log.Logf("udp laddr:%s, raddr:%s", uc.LocalAddr(), uc.RemoteAddr())

	return &socks5UDPConn{UDPConn: uc, taddr: taddr}, nil
}

type socks5UDPTunConnector struct {
	User *url.Userinfo
}

// SOCKS5UDPTunConnector creates a connector for SOCKS5 UDP-over-TCP relay.
// It accepts an optional auth info for SOCKS5 Username/Password Authentication.
func SOCKS5UDPTunConnector(user *url.Userinfo) Connector {
	return &socks5UDPTunConnector{User: user}
}

func (c *socks5UDPTunConnector) Connect(conn net.Conn, address string, options ...ConnectOption) (net.Conn, error) {
	return c.ConnectContext(context.Background(), conn, "udp", address, options...)
}

func (c *socks5UDPTunConnector) ConnectContext(ctx context.Context, conn net.Conn, network, address string, options ...ConnectOption) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil, fmt.Errorf("%s unsupported", network)
	}

	opts := &ConnectOptions{}
	for _, option := range options {
		option(opts)
	}

	user := opts.User
	if user == nil {
		user = c.User
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ConnectTimeout
	}
	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	taddr, _ := net.ResolveUDPAddr("udp", address)
	return newSocks5UDPTunnelConn(conn,
		nil, taddr,
		selectorSocks5HandshakeOption(opts.Selector),
		userSocks5HandshakeOption(user),
		noTLSSocks5HandshakeOption(opts.NoTLS),
	)
}

type socks5Handler struct {
	selector *serverSelector
	options  *HandlerOptions
}

// SOCKS5Handler creates a server Handler for SOCKS5 proxy server.
func SOCKS5Handler(opts ...HandlerOption) Handler {
	h := &socks5Handler{}
	h.Init(opts...)

	return h
}

func (h *socks5Handler) Init(options ...HandlerOption) {
	if h.options == nil {
		h.options = &HandlerOptions{}
	}

	for _, opt := range options {
		opt(h.options)
	}

	tlsConfig := h.options.TLSConfig
	if tlsConfig == nil {
		tlsConfig = DefaultTLSConfig
	}
	h.selector = &serverSelector{ // socks5 server selector
		// Users:     h.options.Users,
		Authenticator: h.options.Authenticator,
		TLSConfig:     tlsConfig,
	}
	// methods that socks5 server supported
	h.selector.AddMethod(
		gosocks5.MethodNoAuth,
		gosocks5.MethodUserPass,
	)
}

func (h *socks5Handler) Handle(conn net.Conn) {
	defer conn.Close()

	conn = gosocks5.ServerConn(conn, h.selector)
	req, err := gosocks5.ReadRequest(conn)
	if err != nil {
		log.Logf("[socks5] %s -> %s : %s",
			conn.RemoteAddr(), conn.LocalAddr(), err)
		return
	}

	if Debug {
		log.Logf("[socks5] %s -> %s\n%s",
			conn.RemoteAddr(), conn.LocalAddr(), req)
	}
	switch req.Cmd {
	case gosocks5.CmdConnect:
		h.handleConnect(conn, req)

	case gosocks5.CmdUdp:
		h.handleUDP(conn, req)

	default:
		log.Logf("[socks5] %s - %s : Unrecognized request: %d",
			conn.RemoteAddr(), conn.LocalAddr(), req.Cmd)
	}
}

func (h *socks5Handler) handleConnect(conn net.Conn, req *gosocks5.Request) {
	host := req.Addr.String()

	log.Logf("[socks5] %s -> %s -> %s",
		conn.RemoteAddr(), h.options.Node.String(), host)

	retries := 1
	if h.options.Chain != nil && h.options.Chain.Retries > 0 {
		retries = h.options.Chain.Retries
	}
	if h.options.Retries > 0 {
		retries = h.options.Retries
	}

	var err error
	var cc net.Conn
	var route *Chain
	for i := 0; i < retries; i++ {
		route, err = h.options.Chain.selectRouteFor(host)
		if err != nil {
			log.Logf("[socks5] %s -> %s : %s",
				conn.RemoteAddr(), conn.LocalAddr(), err)
			continue
		}

		buf := bytes.Buffer{}
		fmt.Fprintf(&buf, "%s -> %s -> ",
			conn.RemoteAddr(), h.options.Node.String())
		for _, nd := range route.route {
			fmt.Fprintf(&buf, "%d@%s -> ", nd.ID, nd.String())
		}
		fmt.Fprintf(&buf, "%s", host)
		log.Log("[route]", buf.String())

		cc, err = route.Dial(host,
			TimeoutChainOption(h.options.Timeout),
		)
		if err == nil {
			break
		}
		log.Logf("[socks5] %s -> %s : %s",
			conn.RemoteAddr(), conn.LocalAddr(), err)
	}

	if err != nil {
		rep := gosocks5.NewReply(gosocks5.HostUnreachable, nil)
		rep.Write(conn)
		if Debug {
			log.Logf("[socks5] %s <- %s\n%s",
				conn.RemoteAddr(), conn.LocalAddr(), rep)
		}
		return
	}
	defer cc.Close()

	rep := gosocks5.NewReply(gosocks5.Succeeded, nil)
	if err := rep.Write(conn); err != nil {
		log.Logf("[socks5] %s <- %s : %s",
			conn.RemoteAddr(), conn.LocalAddr(), err)
		return
	}
	if Debug {
		log.Logf("[socks5] %s <- %s\n%s",
			conn.RemoteAddr(), conn.LocalAddr(), rep)
	}
	log.Logf("[socks5] %s <-> %s", conn.RemoteAddr(), host)
	transport(conn, cc)
	log.Logf("[socks5] %s >-< %s", conn.RemoteAddr(), host)
}

func (h *socks5Handler) discardClientData(conn net.Conn) (err error) {
	b := make([]byte, tinyBufferSize)
	n := 0
	for {
		n, err = conn.Read(b) // discard any data from tcp connection
		if err != nil {
			if err == io.EOF { // disconnect normally
				err = nil
			}
			break // client disconnected
		}
		log.Logf("[socks5-udp] read %d UNEXPECTED TCP data from client", n)
	}
	return
}

func (h *socks5Handler) transportUDP(relay, peer net.PacketConn) (err error) {
	errc := make(chan error, 2)

	var clientAddr net.Addr

	go func() {
		b := mPool.Get().([]byte)
		defer mPool.Put(b)

		for {
			n, laddr, err := relay.ReadFrom(b)
			if err != nil {
				errc <- err
				return
			}
			if clientAddr == nil {
				clientAddr = laddr
			}
			dgram, err := gosocks5.ReadUDPDatagram(bytes.NewReader(b[:n]))
			if err != nil {
				errc <- err
				return
			}

			raddr, err := net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
			if err != nil {
				continue // drop silently
			}
			if _, err := peer.WriteTo(dgram.Data, raddr); err != nil {
				errc <- err
				return
			}
			if Debug {
				log.Logf("[socks5-udp] %s >>> %s length: %d", relay.LocalAddr(), raddr, len(dgram.Data))
			}
		}
	}()

	go func() {
		b := mPool.Get().([]byte)
		defer mPool.Put(b)

		for {
			n, raddr, err := peer.ReadFrom(b)
			if err != nil {
				errc <- err
				return
			}
			if clientAddr == nil {
				continue
			}
			buf := bytes.Buffer{}
			dgram := gosocks5.NewUDPDatagram(gosocks5.NewUDPHeader(0, 0, toSocksAddr(raddr)), b[:n])
			dgram.Write(&buf)
			if _, err := relay.WriteTo(buf.Bytes(), clientAddr); err != nil {
				errc <- err
				return
			}
			if Debug {
				log.Logf("[socks5-udp] %s <<< %s length: %d", relay.LocalAddr(), raddr, len(dgram.Data))
			}
		}
	}()

	select {
	case err = <-errc:
		//log.Println("w exit", err)
	}

	return
}

func (h *socks5Handler) tunnelClientUDP(uc *net.UDPConn, cc net.Conn) (err error) {
	errc := make(chan error, 2)

	var clientAddr *net.UDPAddr

	go func() {
		b := mPool.Get().([]byte)
		defer mPool.Put(b)

		for {
			n, addr, err := uc.ReadFromUDP(b)
			if err != nil {
				log.Logf("[udp-tun] %s <- %s : %s", cc.RemoteAddr(), addr, err)
				errc <- err
				return
			}

			// glog.V(LDEBUG).Infof("read udp %d, % #x", n, b[:n])
			// pipe from relay to tunnel
			dgram, err := gosocks5.ReadUDPDatagram(bytes.NewReader(b[:n]))
			if err != nil {
				errc <- err
				return
			}
			if clientAddr == nil {
				clientAddr = addr
			}
			//raddr := dgram.Header.Addr.String()
			dgram.Header.Rsv = uint16(len(dgram.Data))
			if err := dgram.Write(cc); err != nil {
				errc <- err
				return
			}
			if Debug {
				log.Logf("[udp-tun] %s >>> %s length: %d", uc.LocalAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := gosocks5.ReadUDPDatagram(cc)
			if err != nil {
				log.Logf("[udp-tun] %s -> 0 : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from tunnel to relay
			if clientAddr == nil {
				continue
			}
			//raddr := dgram.Header.Addr.String()
			dgram.Header.Rsv = 0

			buf := bytes.Buffer{}
			dgram.Write(&buf)
			if _, err := uc.WriteToUDP(buf.Bytes(), clientAddr); err != nil {
				errc <- err
				return
			}
			if Debug {
				log.Logf("[udp-tun] %s <<< %s length: %d", uc.LocalAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	select {
	case err = <-errc:
	}

	return
}

func (h *socks5Handler) handleUDP(conn net.Conn, req *gosocks5.Request) {
	// serve tunnel udp, tunnel <-> remote, handle tunnel udp request
	if h.options.Chain.IsEmpty() {
		addr := req.Addr.String()

		bindAddr, _ := net.ResolveUDPAddr("udp", addr)
		uc, err := net.ListenUDP("udp", bindAddr)
		if err != nil {
			log.Logf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
			return
		}
		defer uc.Close()

		socksAddr := toSocksAddr(uc.LocalAddr())
		socksAddr.Host, _, _ = net.SplitHostPort(conn.LocalAddr().String())
		reply := gosocks5.NewReply(gosocks5.Succeeded, socksAddr)
		if err := reply.Write(conn); err != nil {
			log.Logf("[socks5] udp-tun %s <- %s : %s", conn.RemoteAddr(), socksAddr, err)
			return
		}
		if Debug {
			log.Logf("[socks5] udp-tun %s <- %s\n%s", conn.RemoteAddr(), socksAddr, reply)
		}
		log.Logf("[socks5] udp-tun %s <-> %s", conn.RemoteAddr(), socksAddr)
		h.tunnelServerUDP(conn, uc)
		log.Logf("[socks5] udp-tun %s >-< %s", conn.RemoteAddr(), socksAddr)
		return
	}

	cc, err := h.options.Chain.Conn()
	// connection error
	if err != nil {
		log.Logf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		reply := gosocks5.NewReply(gosocks5.Failure, nil)
		reply.Write(conn)
		log.Logf("[socks5] udp-tun %s -> %s\n%s", conn.RemoteAddr(), req.Addr, reply)
		return
	}
	defer cc.Close()

	cc, err = socks5Handshake(cc, userSocks5HandshakeOption(h.options.Chain.LastNode().User))
	if err != nil {
		log.Logf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		return
	}
	// tunnel <-> tunnel, direct forwarding
	// note: this type of request forwarding is defined when starting server
	// so we don't need to authenticate it, as it's as explicit as whitelisting
	//req.Write(cc)
	//log.Logf("--------------------------------------------------------")
	//log.Logf("[socks5] udp-tun %s <-> %s", conn.RemoteAddr(), cc.RemoteAddr())
	//transport(conn, cc)
	//log.Logf("[socks5] udp-tun %s >-< %s", conn.RemoteAddr(), cc.RemoteAddr())
}

func (h *socks5Handler) tunnelServerUDP(cc net.Conn, pc net.PacketConn) (err error) {
	errc := make(chan error, 2)

	go func() {
		b := mPool.Get().([]byte)
		defer mPool.Put(b)

		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				// log.Logf("[udp-tun] %s : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from peer to tunnel
			dgram := gosocks5.NewUDPDatagram(
				gosocks5.NewUDPHeader(uint16(n), 0, toSocksAddr(addr)), b[:n])
			if err := dgram.Write(cc); err != nil {
				log.Logf("[socks5] udp-tun %s <- %s : %s", cc.RemoteAddr(), dgram.Header.Addr, err)
				errc <- err
				return
			}
			if Debug {
				log.Logf("[socks5] udp-tun %s <<< %s length: %d", cc.RemoteAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := gosocks5.ReadUDPDatagram(cc)
			if err != nil {
				// log.Logf("[udp-tun] %s -> 0 : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from tunnel to peer
			addr, err := net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
			if err != nil {
				continue // drop silently
			}
			if _, err := pc.WriteTo(dgram.Data, addr); err != nil {
				log.Logf("[socks5] udp-tun %s -> %s : %s", cc.RemoteAddr(), addr, err)
				errc <- err
				return
			}
			if Debug {
				log.Logf("[socks5] udp-tun %s >>> %s length: %d", cc.RemoteAddr(), addr, len(dgram.Data))
			}
		}
	}()

	select {
	case err = <-errc:
	}

	return
}

// TODO: support ipv6 and domain
func toSocksAddr(addr net.Addr) *gosocks5.Addr {
	host := "0.0.0.0"
	port := 0
	if addr != nil {
		h, p, _ := net.SplitHostPort(addr.String())
		host = h
		port, _ = strconv.Atoi(p)
	}
	return &gosocks5.Addr{
		Type: gosocks5.AddrIPv4,
		Host: host,
		Port: uint16(port),
	}
}

type socks5HandshakeOptions struct {
	selector  gosocks5.Selector
	user      *url.Userinfo
	tlsConfig *tls.Config
	noTLS     bool
}

type socks5HandshakeOption func(opts *socks5HandshakeOptions)

func selectorSocks5HandshakeOption(selector gosocks5.Selector) socks5HandshakeOption {
	return func(opts *socks5HandshakeOptions) {
		opts.selector = selector
	}
}

func userSocks5HandshakeOption(user *url.Userinfo) socks5HandshakeOption {
	return func(opts *socks5HandshakeOptions) {
		opts.user = user
	}
}

func noTLSSocks5HandshakeOption(noTLS bool) socks5HandshakeOption {
	return func(opts *socks5HandshakeOptions) {
		opts.noTLS = noTLS
	}
}

func socks5Handshake(conn net.Conn, opts ...socks5HandshakeOption) (net.Conn, error) {
	options := socks5HandshakeOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	selector := options.selector
	if selector == nil {
		cs := &clientSelector{
			TLSConfig: &tls.Config{InsecureSkipVerify: true},
			User:      options.user,
		}
		cs.AddMethod(
			gosocks5.MethodNoAuth,
			gosocks5.MethodUserPass,
		)
		selector = cs
	}

	cc := gosocks5.ClientConn(conn, selector)
	if err := cc.Handleshake(); err != nil {
		return nil, err
	}
	return cc, nil
}

func getSocks5UDPTunnel(chain *Chain, addr net.Addr) (net.Conn, error) {
	c, err := chain.Conn()
	if err != nil {
		return nil, err
	}

	node := chain.LastNode()
	conn, err := newSocks5UDPTunnelConn(c,
		addr, nil,
		userSocks5HandshakeOption(node.User),
		noTLSSocks5HandshakeOption(node.GetBool("notls")),
	)
	if err != nil {
		c.Close()
	}
	return conn, err
}

type socks5UDPTunnelConn struct {
	net.Conn
	taddr net.Addr
}

func newSocks5UDPTunnelConn(conn net.Conn, raddr, taddr net.Addr, opts ...socks5HandshakeOption) (net.Conn, error) {
	cc, err := socks5Handshake(conn, opts...)
	if err != nil {
		return nil, err
	}

	req := gosocks5.NewRequest(gosocks5.CmdUdp, toSocksAddr(raddr))
	if err := req.Write(cc); err != nil {
		return nil, err
	}
	if Debug {
		log.Log("[socks5] udp-tun", req)
	}

	reply, err := gosocks5.ReadReply(cc)
	if err != nil {
		return nil, err
	}

	if Debug {
		log.Log("[socks5] udp-tun", reply)
	}

	if reply.Rep != gosocks5.Succeeded {
		return nil, errors.New("socks5 UDP tunnel failure")
	}

	baddr, err := net.ResolveUDPAddr("udp", reply.Addr.String())
	if err != nil {
		return nil, err
	}
	log.Logf("[socks5] udp-tun associate on %s OK", baddr)

	return &socks5UDPTunnelConn{
		Conn:  cc,
		taddr: taddr,
	}, nil
}

func (c *socks5UDPTunnelConn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)
	return
}

func (c *socks5UDPTunnelConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	dgram, err := gosocks5.ReadUDPDatagram(c.Conn)
	if err != nil {
		return
	}
	n = copy(b, dgram.Data)
	addr, err = net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
	return
}

func (c *socks5UDPTunnelConn) Write(b []byte) (n int, err error) {
	return c.WriteTo(b, c.taddr)
}

func (c *socks5UDPTunnelConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	dgram := gosocks5.NewUDPDatagram(gosocks5.NewUDPHeader(uint16(len(b)), 0, toSocksAddr(addr)), b)
	if err = dgram.Write(c.Conn); err != nil {
		return
	}
	return len(b), nil
}

type socks5UDPConn struct {
	*net.UDPConn
	taddr net.Addr
}

func (c *socks5UDPConn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)
	return
}

func (c *socks5UDPConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	data := mPool.Get().([]byte)
	defer mPool.Put(data)

	n, err = c.UDPConn.Read(data)
	if err != nil {
		return
	}
	dg, err := gosocks5.ReadUDPDatagram(bytes.NewReader(data[:n]))
	if err != nil {
		return
	}

	n = copy(b, dg.Data)
	addr, err = net.ResolveUDPAddr("udp", dg.Header.Addr.String())

	return
}

func (c *socks5UDPConn) Write(b []byte) (int, error) {
	return c.WriteTo(b, c.taddr)
}

func (c *socks5UDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	adr, err := gosocks5.NewAddr(addr.String())
	if err != nil {
		return 0, err
	}
	h := gosocks5.NewUDPHeader(0, 0, adr)
	dg := gosocks5.NewUDPDatagram(h, b)
	if err = dg.Write(c.UDPConn); err != nil {
		return 0, err
	}
	return len(b), nil
}
