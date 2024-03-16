package core

import (
	"context"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type gvisorUDPOverTCPTunnelConnector struct{}

func GvisorUDPOverTCPTunnelConnector() Connector {
	return &gvisorUDPOverTCPTunnelConnector{}
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
	// 1, get proxy info
	info, err := ParseProxyInfo(tcpConn)
	if err != nil {
		log.Warningf("[TUN-UDP] Error: Failed to parse proxy info: %v", err)
		return
	}
	log.Debugf("[TUN-UDP] Debug: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
		info.LocalPort, info.LocalAddress.String(), info.RemotePort, info.RemoteAddress.String(),
	)
	// 2, dial proxy
	addr := &net.UDPAddr{
		IP:   info.LocalAddress.AsSlice(),
		Port: int(info.LocalPort),
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
		return c.Conn.Read(b)
	}
}

func (c *gvisorFakeUDPTunnelConn) Write(b []byte) (int, error) {
	return c.Conn.Write(b)
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
	log.Debugf("gvisor UDP over TCP listen addr: %s", addr)
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpKeepAliveListener{TCPListener: ln}, nil
}

func handle(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn) {
	defer udpConn.Close()
	log.Debugf("[TUN-UDP] Debug: %s <-> %s", tcpConn.RemoteAddr(), udpConn.RemoteAddr())
	errChan := make(chan error, 2)
	go func() {
		b := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(b[:])

		for ctx.Err() == nil {
			err := tcpConn.SetReadDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				log.Debugf("[TUN-UDP] Error: set read deadline failed: %v", err)
				errChan <- err
				return
			}
			n, err := tcpConn.Read(b[:])
			if err != nil {
				log.Debugf("[TUN-UDP] Debug: %s -> 0 : %v", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			if n == 0 {
				log.Debugf("[TUN-UDP] Error: length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			err = udpConn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				log.Debugf("[TUN-UDP] Error: set write deadline failed: %v", err)
				errChan <- err
				return
			}
			if _, err = udpConn.Write(b[:n]); err != nil {
				log.Debugf("[TUN-UDP] Error: %s -> %s : %s", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			log.Debugf("[TUN-UDP] Debug: %s >>> %s length: %d", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), n)
		}
	}()

	go func() {
		b := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(b[:])

		for ctx.Err() == nil {
			err := udpConn.SetReadDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				log.Debugf("[TUN-UDP] Error: set read deadline failed: %v", err)
				errChan <- err
				return
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
			err = tcpConn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				log.Debugf("[TUN-UDP] Error: set write deadline failed: %v", err)
				errChan <- err
				return
			}
			if _, err = tcpConn.Write(b[:n]); err != nil {
				log.Debugf("[TUN-UDP] Error: %s <- %s : %s", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			log.Debugf("[TUN-UDP] Debug: %s <<< %s length: %d", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), n)
		}
	}()
	err := <-errChan
	if err != nil {
		log.Debugf("[TUN-UDP] Error: %v", err)
	}
	log.Debugf("[TUN-UDP] Debug: %s >-< %s", tcpConn.RemoteAddr(), udpConn.RemoteAddr())
	return
}
