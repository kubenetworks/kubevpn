package core

import (
	"context"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/config"
	"net"
	"time"
)

type fakeUDPTunConnector struct {
}

func UDPOverTCPTunnelConnector() Connector {
	return &fakeUDPTunConnector{}
}

func (c *fakeUDPTunConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	defer conn.SetDeadline(time.Time{})
	return newFakeUDPTunnelConnOverTCP(conn)
}

type fakeUdpHandler struct {
}

// TCPHandler creates a server Handler
func TCPHandler() Handler {
	return &fakeUdpHandler{}
}

func (h *fakeUdpHandler) Handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if config.Debug {
		log.Debugf("[tcpserver] %s -> %s\n", conn.RemoteAddr(), conn.LocalAddr())
	}
	// serve tunnel udp, tunnel <-> remote, handle tunnel udp request
	bindAddr, _ := net.ResolveUDPAddr("udp", ":0")
	uc, err := net.DialUDP("udp", bindAddr, Server8422)
	if err != nil {
		log.Debugf("[tcpserver] udp-tun %s -> %s : %s", conn.RemoteAddr(), bindAddr, err)
		return
	}
	defer uc.Close()
	if config.Debug {
		log.Debugf("[tcpserver] udp-tun %s <- %s\n", conn.RemoteAddr(), uc.LocalAddr())
	}
	log.Debugf("[tcpserver] udp-tun %s <-> %s", conn.RemoteAddr(), uc.LocalAddr())
	_ = h.tunnelServerUDP(conn, uc)
	log.Debugf("[tcpserver] udp-tun %s >-< %s", conn.RemoteAddr(), uc.LocalAddr())
	return
}

var Server8422, _ = net.ResolveUDPAddr("udp", "127.0.0.1:8422")

func (h *fakeUdpHandler) tunnelServerUDP(cc net.Conn, pc *net.UDPConn) (err error) {
	errChan := make(chan error, 2)

	go func() {
		b := MPool.Get().([]byte)
		defer MPool.Put(b)

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
			if config.Debug {
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
			if config.Debug {
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
}

func newFakeUDPTunnelConnOverTCP(conn net.Conn) (net.Conn, error) {
	return &fakeUDPTunnelConn{Conn: conn}, nil
}

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
