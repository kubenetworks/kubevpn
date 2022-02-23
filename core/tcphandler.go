package core

import (
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/util"
	"net"
	"time"
)

type fakeUDPTunConnector struct {
}

// UDPOverTCPTunnelConnector creates a connector for UDP-over-TCP
func UDPOverTCPTunnelConnector() Connector {
	return &fakeUDPTunConnector{}
}

func (c *fakeUDPTunConnector) ConnectContext(_ context.Context, conn net.Conn, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil, fmt.Errorf("%s unsupported", network)
	}
	_ = conn.SetDeadline(time.Now().Add(util.ConnectTimeout))
	defer conn.SetDeadline(time.Time{})

	targetAddr, _ := net.ResolveUDPAddr("udp", address)
	return newFakeUDPTunnelConnOverTCP(conn, targetAddr)
}

type fakeUdpHandler struct {
}

// TCPHandler creates a server Handler
func TCPHandler() Handler {
	return &fakeUdpHandler{}
}

func (h *fakeUdpHandler) Init(...HandlerOption) {
}

func (h *fakeUdpHandler) Handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if util.Debug {
		log.Debugf("[tcpserver] %s -> %s\n", conn.RemoteAddr(), conn.LocalAddr())
	}
	h.handleUDPTunnel(conn)
}

var Server8422, _ = net.ResolveUDPAddr("udp", "127.0.0.1:8422")

func (h *fakeUdpHandler) handleUDPTunnel(conn net.Conn) {
	// serve tunnel udp, tunnel <-> remote, handle tunnel udp request
	bindAddr, _ := net.ResolveUDPAddr("udp", ":0")
	uc, err := net.DialUDP("udp", bindAddr, Server8422)
	if err != nil {
		log.Debugf("[tcpserver] udp-tun %s -> %s : %s", conn.RemoteAddr(), bindAddr, err)
		return
	}
	defer uc.Close()
	if util.Debug {
		log.Debugf("[tcpserver] udp-tun %s <- %s\n", conn.RemoteAddr(), uc.LocalAddr())
	}
	log.Debugf("[tcpserver] udp-tun %s <-> %s", conn.RemoteAddr(), uc.LocalAddr())
	_ = h.tunnelServerUDP(conn, uc)
	log.Debugf("[tcpserver] udp-tun %s >-< %s", conn.RemoteAddr(), uc.LocalAddr())
	return
}

func (h *fakeUdpHandler) tunnelServerUDP(cc net.Conn, pc *net.UDPConn) (err error) {
	errChan := make(chan error, 2)

	go func() {
		b := util.MPool.Get().([]byte)
		defer util.MPool.Put(b)

		for {
			n, err := pc.Read(b)
			if err != nil {
				log.Debugf("[udp-tun] %s : %s", cc.RemoteAddr(), err)
				errChan <- err
				return
			}

			// pipe from peer to tunnel
			dgram := NewDatagramPacket(b[:n])
			if err = dgram.Write(cc); err != nil {
				log.Debugf("[tcpserver] udp-tun %s <- %s : %s", cc.RemoteAddr(), dgram.Addr(), err)
				errChan <- err
				return
			}
			if util.Debug {
				log.Debugf("[tcpserver] udp-tun %s <<< %s length: %d", cc.RemoteAddr(), dgram.Addr(), len(dgram.Data))
			}
		}
	}()

	go func() {
		for {
			dgram, err := ReadDatagramPacket(cc)
			if err != nil {
				log.Debugf("[udp-tun] %s -> 0 : %v", cc.RemoteAddr(), err)
				errChan <- err
				return
			}

			if _, err = pc.Write(dgram.Data); err != nil {
				log.Debugf("[tcpserver] udp-tun %s -> %s : %s", cc.RemoteAddr(), Server8422, err)
				errChan <- err
				return
			}
			if util.Debug {
				log.Debugf("[tcpserver] udp-tun %s >>> %s length: %d", cc.RemoteAddr(), Server8422, len(dgram.Data))
			}
		}
	}()

	return <-errChan
}

// fake udp connect over tcp
type fakeUDPTunnelConn struct {
	// tcp connection
	net.Conn
	targetAddr net.Addr
}

func newFakeUDPTunnelConnOverTCP(conn net.Conn, targetAddr net.Addr) (net.Conn, error) {
	return &fakeUDPTunnelConn{
		Conn:       conn,
		targetAddr: targetAddr,
	}, nil
}

//func (c *fakeUDPTunnelConn) Read(b []byte) (n int, err error) {
//	n, _, err = c.ReadFrom(b)
//	return
//}

func (c *fakeUDPTunnelConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	dgram, err := ReadDatagramPacket(c.Conn)
	if err != nil {
		log.Debug(err)
		return
	}
	n = copy(b, dgram.Data)
	addr = dgram.Addr()
	return
}

//func (c *fakeUDPTunnelConn) Write(b []byte) (n int, err error) {
//	return c.WriteTo(b, nil)
//}

func (c *fakeUDPTunnelConn) WriteTo(b []byte, _ net.Addr) (n int, err error) {
	dgram := NewDatagramPacket(b)
	if err = dgram.Write(c.Conn); err != nil {
		return
	}
	return len(b), nil
}

func (c *fakeUDPTunnelConn) Close() error {
	return c.Conn.Close()
}

func (c *fakeUDPTunnelConn) CloseWrite() error {
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cc.CloseWrite()
	}
	return nil
}

func (c *fakeUDPTunnelConn) CloseRead() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return cc.CloseRead()
	}
	return nil
}
