package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

type GvisorFakeUDPTunnelConnector struct {
	Id stack.TransportEndpointID
}

func GvisorUDPOverTCPTunnelConnector(endpointID stack.TransportEndpointID) Connector {
	return &GvisorFakeUDPTunnelConnector{
		Id: endpointID,
	}
}

func (c *GvisorFakeUDPTunnelConnector) ConnectContext(ctx context.Context, conn net.Conn) (net.Conn, error) {
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
	func(conn net.Conn) {
		defer conn.Close()
		// 1, get proxy info
		endpointID, err := ParseProxyInfo(conn)
		if err != nil {
			log.Warningf("[TUN-UDP] ERROR Failed to parse proxy info: %v", err)
			return
		}
		log.Infof("[TUN-UDP] Info: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			endpointID.LocalPort, endpointID.LocalAddress.String(), endpointID.RemotePort, endpointID.RemoteAddress.String(),
		)
		// 2, dial proxy
		var dial *net.UDPConn
		// todo 发送到 localhost:8422 ? 🤔
		addr := &net.UDPAddr{
			IP:   endpointID.LocalAddress.AsSlice(),
			Port: int(endpointID.LocalPort),
		}
		dial, err = net.DialUDP("udp", nil, addr)
		if err != nil {
			log.Warningln(err)
			return
		}
		h.HandleInner(ctx, tcpConn, dial)
	}(tcpConn)
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
		return 0, errors.New("closed connection")
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

func copyPacketData(dst, src net.PacketConn, to net.Addr, timeout time.Duration) error {
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])

	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, _, err := src.ReadFrom(buf)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil /* ignore I/O timeout */
		} else if err == io.EOF {
			return nil /* ignore EOF */
		} else if err != nil {
			return err
		}

		if _, err = dst.WriteTo(buf[:n], to); err != nil {
			return err
		}
		dst.SetReadDeadline(time.Now().Add(timeout))
	}
}

func (h *gvisorUDPHandler) HandleInner(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn) {
	defer udpConn.Close()
	log.Debugf("[TUN-UDP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	log.Debugf("[TUN-UDP] udp-tun %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	errChan := make(chan error, 2)
	go func() {
		b := config.LPool.Get().([]byte)
		defer config.LPool.Put(b[:])

		for {
			dgram, err := readDatagramPacket(tcpConn, b[:])
			if err != nil {
				log.Debugf("[TUN-UDP] %s -> 0 : %v", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			if dgram.DataLength == 0 {
				log.Debugf("[TUN-UDP] length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			if _, err = udpConn.Write(dgram.Data); err != nil {
				log.Debugf("[TUN-UDP] udp-tun %s -> %s : %s", tcpConn.RemoteAddr(), Server8422, err)
				errChan <- err
				return
			}
			log.Debugf("[TUN-UDP] udp-tun %s >>> %s length: %d", tcpConn.RemoteAddr(), Server8422, dgram.DataLength)
		}
	}()

	go func() {
		b := config.LPool.Get().([]byte)
		defer config.LPool.Put(b[:])

		for {
			n, _, err := udpConn.ReadFrom(b[:])
			if err != nil {
				log.Debugf("[TUN-UDP] %s : %s", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			if n == 0 {
				log.Debugf("[TUN-UDP] length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			// pipe from peer to tunnel
			dgram := newDatagramPacket(b[:n])
			if err = dgram.Write(tcpConn); err != nil {
				log.Debugf("[TUN-UDP] udp-tun %s <- %s : %s", tcpConn.RemoteAddr(), dgram.Addr(), err)
				errChan <- err
				return
			}
			log.Debugf("[TUN-UDP] udp-tun %s <<< %s length: %d", tcpConn.RemoteAddr(), dgram.Addr(), len(dgram.Data))
		}
	}()
	err := <-errChan
	if err != nil {
		log.Error(err)
	}
	log.Debugf("[TUN-UDP] udp-tun %s >-< %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	return
}
