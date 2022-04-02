package core

import (
	"context"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/config"
	"net"
	"time"
)

type fakeUDPTunnelConnector struct {
}

func UDPOverTCPTunnelConnector() Connector {
	return &fakeUDPTunnelConnector{}
}

func (c *fakeUDPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	defer conn.SetDeadline(time.Time{})
	return newFakeUDPTunnelConnOverTCP(conn)
}

type fakeUdpHandler struct {
}

// TCPHandler creates a server Handler
func TCPHandler() Handler {
	return &fakeUdpHandler{}
}

func (h *fakeUdpHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	if config.Debug {
		log.Debugf("[tcpserver] %s -> %s\n", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	}
	// serve tunnel udp, tunnel <-> remote, handle tunnel udp request
	udpConn, err := net.DialUDP("udp", nil, Server8422)
	if err != nil {
		log.Debugf("[tcpserver] udp-tun %s -> %s : %s", tcpConn.RemoteAddr(), udpConn.LocalAddr(), err)
		return
	}
	defer udpConn.Close()
	if config.Debug {
		log.Debugf("[tcpserver] udp-tun %s <- %s\n", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	}
	log.Debugf("[tcpserver] udp-tun %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	_ = h.tunnelServerUDP(tcpConn, udpConn)
	log.Debugf("[tcpserver] udp-tun %s >-< %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	return
}

var Server8422, _ = net.ResolveUDPAddr("udp", "127.0.0.1:8422")

func (h *fakeUdpHandler) tunnelServerUDP(tcpConn net.Conn, udpConn *net.UDPConn) (err error) {
	errChan := make(chan error, 2)
	go func() {
		for {
			dgram, err := ReadDatagramPacket(tcpConn)
			if err != nil {
				log.Debugf("[udp-tun] %s -> 0 : %v", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}

			if _, err = udpConn.Write(dgram.Data); err != nil {
				log.Debugf("[tcpserver] udp-tun %s -> %s : %s", tcpConn.RemoteAddr(), Server8422, err)
				errChan <- err
				return
			}
			if config.Debug {
				log.Debugf("[tcpserver] udp-tun %s >>> %s length: %d", tcpConn.RemoteAddr(), Server8422, len(dgram.Data))
			}
		}
	}()

	go func() {
		b := MPool.Get().([]byte)
		defer MPool.Put(b)

		for {
			n, err := udpConn.Read(b)
			if err != nil {
				log.Debugf("[udp-tun] %s : %s", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}

			// pipe from peer to tunnel
			dgram := NewDatagramPacket(b[:n])
			if err = dgram.Write(tcpConn); err != nil {
				log.Debugf("[tcpserver] udp-tun %s <- %s : %s", tcpConn.RemoteAddr(), dgram.Addr(), err)
				errChan <- err
				return
			}
			if config.Debug {
				log.Debugf("[tcpserver] udp-tun %s <<< %s length: %d", tcpConn.RemoteAddr(), dgram.Addr(), len(dgram.Data))
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
