package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"kubevpn/util"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/ginuerzh/gosocks5"
	log "github.com/sirupsen/logrus"
)

type clientSelector struct {
	methods []uint8
}

func (selector *clientSelector) Methods() []uint8 {
	if util.Debug {
		log.Debug("[socks5] methods:", selector.methods)
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
	if util.Debug {
		log.Debug("[socks5] method selected:", method)
	}
	switch method {
	case gosocks5.MethodNoAuth:
		log.Debug("[socks5] client", "no auth")
		return conn, nil
	}

	return conn, nil
}

type serverSelector struct {
	methods []uint8
}

func (selector *serverSelector) Methods() []uint8 {
	return selector.methods
}

func (selector *serverSelector) AddMethod(methods ...uint8) {
	selector.methods = append(selector.methods, methods...)
}

func (selector *serverSelector) Select(methods ...uint8) (method uint8) {
	if util.Debug {
		log.Debugf("[socks5] %d %d %v", gosocks5.Ver5, len(methods), methods)
	}
	method = gosocks5.MethodNoAuth
	return
}

func (selector *serverSelector) OnSelected(method uint8, conn net.Conn) (net.Conn, error) {
	if util.Debug {
		log.Debugf("[socks5] %d %d", gosocks5.Ver5, method)
	}
	switch method {
	case gosocks5.MethodNoAuth:
		log.Debugf("[socks5] server, no auth")
		return conn, nil
	case gosocks5.MethodNoAcceptable:
		return nil, gosocks5.ErrBadMethod
	}

	return conn, nil
}

type socks5UDPTunConnector struct {
}

// SOCKS5UDPTunConnector creates a connector for SOCKS5 UDP-over-TCP relay.
// It accepts an optional auth info for SOCKS5 Username/Password Authentication.
func SOCKS5UDPTunConnector() Connector {
	return &socks5UDPTunConnector{}
}

func (c *socks5UDPTunConnector) ConnectContext(ctx context.Context, conn net.Conn, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil, fmt.Errorf("%s unsupported", network)
	}
	conn.SetDeadline(time.Now().Add(util.ConnectTimeout))
	defer conn.SetDeadline(time.Time{})

	taddr, _ := net.ResolveUDPAddr("udp", address)
	return newSocks5UDPTunnelConn(conn, nil, taddr)
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

	h.selector = &serverSelector{}
	h.selector.AddMethod(
		gosocks5.MethodNoAuth,
	)
}

func (h *socks5Handler) Handle(conn net.Conn) {
	defer conn.Close()

	conn = gosocks5.ServerConn(conn, h.selector)
	req, err := gosocks5.ReadRequest(conn)
	if err != nil {
		log.Debugf("[socks5] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
		return
	}

	if util.Debug {
		log.Debugf("[socks5] %s -> %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), req)
	}
	switch req.Cmd {
	case gosocks5.CmdConnect:
		h.handleConnect(conn, req)

	//case gosocks5.CmdBind:
	//	h.handleBind(conn, req)
	//
	case gosocks5.CmdUdp:
		h.handleUDPTunnel(conn, req)

	default:
		log.Debugf("[socks5] %s - %s : Unrecognized request: %d", conn.RemoteAddr(), conn.LocalAddr(), req.Cmd)
	}
}

func (h *socks5Handler) handleConnect(conn net.Conn, req *gosocks5.Request) {
	host := req.Addr.String()

	log.Debugf("[socks5] %s -> %s -> %s", conn.RemoteAddr(), h.options.Node.String(), host)

	retries := 1
	if h.options.Chain != nil && h.options.Chain.Retries > 0 {
		retries = h.options.Chain.Retries
	}

	var err error
	var cc net.Conn
	var route *Chain
	for i := 0; i < retries; i++ {
		route, err = h.options.Chain.selectRouteFor(host)
		if err != nil {
			log.Debugf("[socks5] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
			continue
		}

		buf := bytes.Buffer{}
		_, _ = fmt.Fprintf(&buf, "%s -> %s -> ", conn.RemoteAddr(), h.options.Node.String())
		_, _ = fmt.Fprintf(&buf, "%s -> ", route.node.String())
		_, _ = fmt.Fprintf(&buf, "%s", host)
		log.Debug("[route]", buf.String())

		cc, err = route.DialContext(context.Background(), "tcp", host)
		if err == nil {
			break
		}
		log.Debugf("[socks5] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
	}

	if err != nil {
		rep := gosocks5.NewReply(gosocks5.HostUnreachable, nil)
		rep.Write(conn)
		if util.Debug {
			log.Debugf("[socks5] %s <- %s\n%s",
				conn.RemoteAddr(), conn.LocalAddr(), rep)
		}
		return
	}
	defer cc.Close()

	rep := gosocks5.NewReply(gosocks5.Succeeded, nil)
	if err := rep.Write(conn); err != nil {
		log.Debugf("[socks5] %s <- %s : %s",
			conn.RemoteAddr(), conn.LocalAddr(), err)
		return
	}
	if util.Debug {
		log.Debugf("[socks5] %s <- %s\n%s",
			conn.RemoteAddr(), conn.LocalAddr(), rep)
	}
	log.Debugf("[socks5] %s <-> %s", conn.RemoteAddr(), host)
	transport(conn, cc)
	log.Debugf("[socks5] %s >-< %s", conn.RemoteAddr(), host)
}

func (h *socks5Handler) handleBind(conn net.Conn, req *gosocks5.Request) {
	addr := req.Addr.String()

	log.Debugf("[socks5-bind] %s -> %s -> %s", conn.RemoteAddr(), h.options.Node.String(), addr)

	if h.options.Chain.IsEmpty() {
		h.bindOn(conn, addr)
		return
	}

	cc, err := h.options.Chain.Conn()
	if err != nil {
		log.Debugf("[socks5-bind] %s <- %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
		reply := gosocks5.NewReply(gosocks5.Failure, nil)
		reply.Write(conn)
		if util.Debug {
			log.Debugf("[socks5-bind] %s <- %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), reply)
		}
		return
	}

	// forward request
	// note: this type of request forwarding is defined when starting server,
	// so we don't need to authenticate it, as it's as explicit as whitelisting
	defer cc.Close()
	req.Write(cc)
	log.Debugf("[socks5-bind] %s <-> %s", conn.RemoteAddr(), addr)
	transport(conn, cc)
	log.Debugf("[socks5-bind] %s >-< %s", conn.RemoteAddr(), addr)
}

func (h *socks5Handler) bindOn(conn net.Conn, addr string) {
	bindAddr, _ := net.ResolveTCPAddr("tcp", addr)
	ln, err := net.ListenTCP("tcp", bindAddr) // strict mode: if the port already in use, it will return error
	if err != nil {
		log.Debugf("[socks5-bind] %s -> %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
		gosocks5.NewReply(gosocks5.Failure, nil).Write(conn)
		return
	}

	socksAddr := toSocksAddr(ln.Addr())
	// Issue: may not reachable when host has multi-interface
	socksAddr.Host, _, _ = net.SplitHostPort(conn.LocalAddr().String())
	reply := gosocks5.NewReply(gosocks5.Succeeded, socksAddr)
	if err := reply.Write(conn); err != nil {
		log.Debugf("[socks5-bind] %s <- %s : %s", conn.RemoteAddr(), conn.LocalAddr(), err)
		ln.Close()
		return
	}
	if util.Debug {
		log.Debugf("[socks5-bind] %s <- %s\n%s", conn.RemoteAddr(), conn.LocalAddr(), reply)
	}
	log.Debugf("[socks5-bind] %s - %s BIND ON %s OK", conn.RemoteAddr(), conn.LocalAddr(), socksAddr)

	var pconn net.Conn
	accept := func() <-chan error {
		errc := make(chan error, 1)

		go func() {
			defer close(errc)
			defer ln.Close()

			c, err := ln.AcceptTCP()
			if err != nil {
				errc <- err
				return
			}
			pconn = c
		}()

		return errc
	}

	pc1, pc2 := net.Pipe()
	pipe := func() <-chan error {
		errc := make(chan error, 1)

		go func() {
			defer close(errc)
			defer pc1.Close()

			errc <- transport(conn, pc1)
		}()

		return errc
	}

	defer pc2.Close()

	for {
		select {
		case err := <-accept():
			if err != nil || pconn == nil {
				log.Debugf("[socks5-bind] %s <- %s : %v", conn.RemoteAddr(), addr, err)
				return
			}
			defer pconn.Close()

			reply := gosocks5.NewReply(gosocks5.Succeeded, toSocksAddr(pconn.RemoteAddr()))
			if err := reply.Write(pc2); err != nil {
				log.Debugf("[socks5-bind] %s <- %s : %v", conn.RemoteAddr(), addr, err)
			}
			if util.Debug {
				log.Debugf("[socks5-bind] %s <- %s\n%s", conn.RemoteAddr(), addr, reply)
			}
			log.Debugf("[socks5-bind] %s <- %s PEER %s ACCEPTED", conn.RemoteAddr(), socksAddr, pconn.RemoteAddr())

			log.Debugf("[socks5-bind] %s <-> %s", conn.RemoteAddr(), pconn.RemoteAddr())
			if err = transport(pc2, pconn); err != nil {
				log.Debugf("[socks5-bind] %s - %s : %v", conn.RemoteAddr(), pconn.RemoteAddr(), err)
			}
			log.Debugf("[socks5-bind] %s >-< %s", conn.RemoteAddr(), pconn.RemoteAddr())
			return
		case err := <-pipe():
			if err != nil {
				log.Debugf("[socks5-bind] %s -> %s : %v", conn.RemoteAddr(), addr, err)
			}
			ln.Close()
			return
		}
	}
}

func (h *socks5Handler) transportUDP(relay, peer net.PacketConn) (err error) {
	errc := make(chan error, 2)

	var clientAddr net.Addr

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

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
			if util.Debug {
				log.Debugf("[socks5-udp] %s >>> %s length: %d", relay.LocalAddr(), raddr, len(dgram.Data))
			}
		}
	}()

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

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
			if util.Debug {
				log.Debugf("[socks5-udp] %s <<< %s length: %d", relay.LocalAddr(), raddr, len(dgram.Data))
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
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, addr, err := uc.ReadFromUDP(b)
			if err != nil {
				log.Debugf("[udp-tun] %s <- %s : %s", cc.RemoteAddr(), addr, err)
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
			if util.Debug {
				log.Debugf("[udp-tun] %s >>> %s length: %d", uc.LocalAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := gosocks5.ReadUDPDatagram(cc)
			if err != nil {
				log.Debugf("[udp-tun] %s -> 0 : %s", cc.RemoteAddr(), err)
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
			if util.Debug {
				log.Debugf("[udp-tun] %s <<< %s length: %d", uc.LocalAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	select {
	case err = <-errc:
	}

	return
}

func (h *socks5Handler) handleUDPTunnel(conn net.Conn, req *gosocks5.Request) {
	// serve tunnel udp, tunnel <-> remote, handle tunnel udp request
	if h.options.Chain.IsEmpty() {
		addr := req.Addr.String()

		bindAddr, _ := net.ResolveUDPAddr("udp", addr)
		uc, err := net.ListenUDP("udp", bindAddr)
		if err != nil {
			log.Debugf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
			return
		}
		defer uc.Close()

		socksAddr := toSocksAddr(uc.LocalAddr())
		socksAddr.Host, _, _ = net.SplitHostPort(conn.LocalAddr().String())
		reply := gosocks5.NewReply(gosocks5.Succeeded, socksAddr)
		if err := reply.Write(conn); err != nil {
			log.Debugf("[socks5] udp-tun %s <- %s : %s", conn.RemoteAddr(), socksAddr, err)
			return
		}
		if util.Debug {
			log.Debugf("[socks5] udp-tun %s <- %s\n%s", conn.RemoteAddr(), socksAddr, reply)
		}
		log.Debugf("[socks5] udp-tun %s <-> %s", conn.RemoteAddr(), socksAddr)
		h.tunnelServerUDP(conn, uc)
		log.Debugf("[socks5] udp-tun %s >-< %s", conn.RemoteAddr(), socksAddr)
		return
	}

	cc, err := h.options.Chain.Conn()
	// connection error
	if err != nil {
		log.Debugf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		reply := gosocks5.NewReply(gosocks5.Failure, nil)
		reply.Write(conn)
		log.Debugf("[socks5] udp-tun %s -> %s\n%s", conn.RemoteAddr(), req.Addr, reply)
		return
	}
	defer cc.Close()

	cc, err = socks5Handshake(cc)
	if err != nil {
		log.Debugf("[socks5] udp-tun %s -> %s : %s", conn.RemoteAddr(), req.Addr, err)
		return
	}
	// tunnel <-> tunnel, direct forwarding
	// note: this type of request forwarding is defined when starting server
	// so we don't need to authenticate it, as it's as explicit as whitelisting
	req.Write(cc)

	log.Debugf("[socks5] udp-tun %s <-> %s", conn.RemoteAddr(), cc.RemoteAddr())
	transport(conn, cc)
	log.Debugf("[socks5] udp-tun %s >-< %s", conn.RemoteAddr(), cc.RemoteAddr())
}

func (h *socks5Handler) tunnelServerUDP(cc net.Conn, pc net.PacketConn) (err error) {
	errc := make(chan error, 2)

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				// log.Debugf("[udp-tun] %s : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from peer to tunnel
			dgram := gosocks5.NewUDPDatagram(
				gosocks5.NewUDPHeader(uint16(n), 0, toSocksAddr(addr)), b[:n])
			if err := dgram.Write(cc); err != nil {
				log.Debugf("[socks5] udp-tun %s <- %s : %s", cc.RemoteAddr(), dgram.Header.Addr, err)
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[socks5] udp-tun %s <<< %s length: %d", cc.RemoteAddr(), dgram.Header.Addr, len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := gosocks5.ReadUDPDatagram(cc)
			if err != nil {
				// log.Debugf("[udp-tun] %s -> 0 : %s", cc.RemoteAddr(), err)
				errc <- err
				return
			}

			// pipe from tunnel to peer
			addr, err := net.ResolveUDPAddr("udp", dgram.Header.Addr.String())
			if err != nil {
				continue // drop silently
			}
			if _, err := pc.WriteTo(dgram.Data, addr); err != nil {
				log.Debugf("[socks5] udp-tun %s -> %s : %s", cc.RemoteAddr(), addr, err)
				errc <- err
				return
			}
			if util.Debug {
				log.Debugf("[socks5] udp-tun %s >>> %s length: %d", cc.RemoteAddr(), addr, len(dgram.Data))
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
	selector gosocks5.Selector
	user     *url.Userinfo
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

func socks5Handshake(conn net.Conn) (net.Conn, error) {
	cs := &clientSelector{}
	cs.AddMethod(
		gosocks5.MethodNoAuth,
	)

	cc := gosocks5.ClientConn(conn, cs)
	if err := cc.Handleshake(); err != nil {
		return nil, err
	}
	return cc, nil
}

type socks5UDPTunnelConn struct {
	net.Conn
	taddr net.Addr
}

func newSocks5UDPTunnelConn(conn net.Conn, raddr, taddr net.Addr) (net.Conn, error) {
	cc, err := socks5Handshake(conn)
	if err != nil {
		return nil, err
	}

	req := gosocks5.NewRequest(gosocks5.CmdUdp, toSocksAddr(raddr))
	if err := req.Write(cc); err != nil {
		return nil, err
	}
	if util.Debug {
		log.Debug("[socks5] udp-tun", req)
	}

	reply, err := gosocks5.ReadReply(cc)
	if err != nil {
		return nil, err
	}

	if util.Debug {
		log.Debug("[socks5] udp-tun", reply)
	}

	if reply.Rep != gosocks5.Succeeded {
		return nil, errors.New("socks5 UDP tunnel failure")
	}

	baddr, err := net.ResolveUDPAddr("udp", reply.Addr.String())
	if err != nil {
		return nil, err
	}
	log.Debugf("[socks5] udp-tun associate on %s OK", baddr)

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
