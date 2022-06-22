package core

import (
	"context"
	"errors"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/config"
)

type fakeUDPTunnelConnector struct {
}

func UDPOverTCPTunnelConnector() Connector {
	return &fakeUDPTunnelConnector{}
}

func (c *fakeUDPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	defer conn.SetDeadline(time.Time{})
	return newFakeUDPTunnelConnOverTCP(ctx, conn)
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
		b := LPool.Get().([]byte)
		defer LPool.Put(b)

		for {
			dgram, err := readDatagramPacket(tcpConn, b[:])
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
			n, err := udpConn.Read(b[:])
			if err != nil {
				log.Debugf("[udp-tun] %s : %s", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}

			// pipe from peer to tunnel
			dgram := newDatagramPacket(b[:n])
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
	ctx context.Context
}

func newFakeUDPTunnelConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &fakeUDPTunnelConn{ctx: ctx, Conn: conn}, nil
}

func (c *fakeUDPTunnelConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-c.ctx.Done():
		return 0, nil, errors.New("closed connection")
	default:
		dgram, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, nil, err
		}
		return int(dgram.DataLength), dgram.Addr(), nil
	}
}

func (c *fakeUDPTunnelConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	dgram := newDatagramPacket(b)
	if err := dgram.Write(c.Conn); err != nil {
		return 0, err
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
