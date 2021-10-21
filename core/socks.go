package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/wencaiwulue/kubevpn/util"
	"net"
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

	targetAddr, _ := net.ResolveUDPAddr("udp", address)
	return newSocks5UDPTunnelConn(conn, nil, targetAddr)
}

type socks5Handler struct {
	selector *serverSelector
}

// SOCKS5Handler creates a server Handler for SOCKS5 proxy server.
func SOCKS5Handler() Handler {
	h := &socks5Handler{}
	h.Init()

	return h
}

func (h *socks5Handler) Init(...HandlerOption) {
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
	case gosocks5.CmdUdp:
		h.handleUDPTunnel(conn, req)

	default:
		log.Debugf("[socks5] %s - %s : Unrecognized request: %d", conn.RemoteAddr(), conn.LocalAddr(), req.Cmd)
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

// fake upd connect over tcp
type socks5UDPTunnelConn struct {
	// tcp connection
	net.Conn
	targetAddr net.Addr
}

func newSocks5UDPTunnelConn(conn net.Conn, serverAddr, targetAddr net.Addr) (net.Conn, error) {
	cc, err := socks5Handshake(conn)
	if err != nil {
		return nil, err
	}

	req := gosocks5.NewRequest(gosocks5.CmdUdp, toSocksAddr(serverAddr))
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

	bindAddr, err := net.ResolveUDPAddr("udp", reply.Addr.String())
	if err != nil {
		return nil, err
	}
	log.Debugf("[socks5] udp-tun associate on %s OK", bindAddr)

	return &socks5UDPTunnelConn{
		Conn:       cc,
		targetAddr: targetAddr,
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
	return c.WriteTo(b, c.targetAddr)
}

func (c *socks5UDPTunnelConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	dgram := gosocks5.NewUDPDatagram(gosocks5.NewUDPHeader(uint16(len(b)), 0, toSocksAddr(addr)), b)
	if err = dgram.Write(c.Conn); err != nil {
		return
	}
	return len(b), nil
}
