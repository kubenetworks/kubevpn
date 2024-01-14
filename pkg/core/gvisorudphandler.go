package core

import (
	"context"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type gvisorUDPOverTCPTunnelConnector struct {
	Id stack.TransportEndpointID
}

func GvisorUDPOverTCPTunnelConnector(endpointID stack.TransportEndpointID) Connector {
	return &gvisorUDPOverTCPTunnelConnector{
		Id: endpointID,
	}
}

func (c *gvisorUDPOverTCPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
	switch con := conn.(type) {
	case *net.TCPConn:
		err := con.SetNoDelay(true)
		if err != nil {
			return nil, err
		}
		err = con.SetKeepAlive(true)
		if err != nil {
			return nil, err
		}
		err = con.SetKeepAlivePeriod(15 * time.Second)
		if err != nil {
			return nil, err
		}
	}
	return newGvisorFakeUDPTunnelConnOverTCP(ctx, conn)
}

type gvisorUDPHandler struct{}

func GvisorUDPHandler() Handler {
	return &gvisorUDPHandler{}
}

func (h *gvisorUDPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	log.Debugf("[TUN-UDP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	// 1, get proxy info
	endpointID, err := ParseProxyInfo(tcpConn)
	if err != nil {
		log.Warningf("[TUN-UDP] Error: Failed to parse proxy info: %v", err)
		return
	}
	log.Debugf("[TUN-UDP] Debug: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
		endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
	)
	// 2, dial proxy
	addr := &net.UDPAddr{
		IP:   endpointID.LocalAddress.AsSlice(),
		Port: int(endpointID.LocalPort),
	}
	var remote *net.UDPConn
	remote, err = net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Debugf("[TUN-UDP] Error: failed to connect addr %s: %v", addr.String(), err)
		return
	}
	handle(ctx, tcpConn, remote)
}

// fake udp connect over tcp
type gvisorFakeUDPTunnelConn struct {
	// tcp connection
	net.Conn
	ctx context.Context
}

func newGvisorFakeUDPTunnelConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &gvisorFakeUDPTunnelConn{ctx: ctx, Conn: conn}, nil
}

func (c *gvisorFakeUDPTunnelConn) Read(b []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		dgram, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, err
		}
		return int(dgram.DataLength), nil
	}
}

func (c *gvisorFakeUDPTunnelConn) Write(b []byte) (int, error) {
	dgram := newDatagramPacket(b)
	if err := dgram.Write(c.Conn); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *gvisorFakeUDPTunnelConn) Close() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		_ = cc.CloseRead()
	}
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cc.CloseWrite()
	}
	return c.Conn.Close()
}

func GvisorUDPListener(addr string) (net.Listener, error) {
	log.Debug("gvisor UDP over TCP listen addr", addr)
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpKeepAliveListener{ln}, nil
}

func handle(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn) {
	defer udpConn.Close()
	log.Debugf("[TUN-UDP] Debug: %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	errChan := make(chan error, 2)
	go func() {
		b := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(b[:])

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			dgram, err := readDatagramPacket(tcpConn, b[:])
			if err != nil {
				log.Debugf("[TUN-UDP] Debug: %s -> 0 : %v", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			if dgram.DataLength == 0 {
				log.Debugf("[TUN-UDP] Error: length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			if _, err = udpConn.Write(dgram.Data); err != nil {
				log.Debugf("[TUN-UDP] Error: %s -> %s : %s", tcpConn.RemoteAddr(), "localhost:8422", err)
				errChan <- err
				return
			}
			log.Debugf("[TUN-UDP] Debug: %s >>> %s length: %d", tcpConn.RemoteAddr(), "localhost:8422", dgram.DataLength)
		}
	}()

	go func() {
		b := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(b[:])

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			n, _, err := udpConn.ReadFrom(b[:])
			if err != nil {
				log.Debugf("[TUN-UDP] Error: %s : %s", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			if n == 0 {
				log.Debugf("[TUN-UDP] Error: length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			// pipe from peer to tunnel
			dgram := newDatagramPacket(b[:n])
			if err = dgram.Write(tcpConn); err != nil {
				log.Debugf("[TUN-UDP] Error: %s <- %s : %s", tcpConn.RemoteAddr(), dgram.Addr(), err)
				errChan <- err
				return
			}
			log.Debugf("[TUN-UDP] Debug: %s <<< %s length: %d", tcpConn.RemoteAddr(), dgram.Addr(), len(dgram.Data))
		}
	}()
	err := <-errChan
	if err != nil {
		log.Debugf("[TUN-UDP] Error: %v", err)
	}
	log.Debugf("[TUN-UDP] Debug: %s >-< %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	return
}
