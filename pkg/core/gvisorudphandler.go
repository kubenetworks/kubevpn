package core

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type gvisorUDPHandler struct{}

func GvisorUDPHandler() Handler {
	return &gvisorUDPHandler{}
}

func (h *gvisorUDPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	plog.G(ctx).Infof("[TUN-UDP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	// 1, get proxy info
	endpointID, err := ParseProxyInfo(tcpConn)
	if err != nil {
		plog.G(ctx).Errorf("[TUN-UDP] Failed to parse proxy info: %v", err)
		return
	}
	plog.G(ctx).Infof("[TUN-UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
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
		plog.G(ctx).Errorf("[TUN-UDP] Failed to connect addr %s: %v", addr.String(), err)
		return
	}
	handle(ctx, tcpConn, remote)
}

// fake udp connect over tcp
type gvisorUDPConnOverTCP struct {
	// tcp connection
	net.Conn
	ctx context.Context
}

func newGvisorUDPConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &gvisorUDPConnOverTCP{ctx: ctx, Conn: conn}, nil
}

func (c *gvisorUDPConnOverTCP) Read(b []byte) (int, error) {
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

func (c *gvisorUDPConnOverTCP) Write(b []byte) (int, error) {
	packet := newDatagramPacket(b)
	if err := packet.Write(c.Conn); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *gvisorUDPConnOverTCP) Close() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		_ = cc.CloseRead()
	}
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cc.CloseWrite()
	}
	return c.Conn.Close()
}

func GvisorUDPListener(addr string) (net.Listener, error) {
	plog.G(context.Background()).Infof("Gvisor UDP over TCP listening addr: %s", addr)
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
	plog.G(ctx).Infof("[TUN-UDP] %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	errChan := make(chan error, 2)
	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := tcpConn.SetReadDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] Failed to set read deadline: %v", err)
				errChan <- err
				return
			}
			dgram, err := readDatagramPacket(tcpConn, buf[:])
			if err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] %s -> %s: %v", tcpConn.RemoteAddr(), udpConn.LocalAddr(), err)
				errChan <- err
				return
			}
			if dgram.DataLength == 0 {
				plog.G(ctx).Errorf("[TUN-UDP] Length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			err = udpConn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] Failed to set write deadline: %v", err)
				errChan <- err
				return
			}
			if _, err = udpConn.Write(dgram.Data); err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] %s -> %s : %s", tcpConn.RemoteAddr(), "localhost:8422", err)
				errChan <- err
				return
			}
			plog.G(ctx).Infof("[TUN-UDP] %s >>> %s length: %d", tcpConn.RemoteAddr(), "localhost:8422", dgram.DataLength)
		}
	}()

	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := udpConn.SetReadDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] Failed to set read deadline failed: %v", err)
				errChan <- err
				return
			}
			n, _, err := udpConn.ReadFrom(buf[:])
			if err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] %s : %s", tcpConn.RemoteAddr(), err)
				errChan <- err
				return
			}
			if n == 0 {
				plog.G(ctx).Errorf("[TUN-UDP] Length is zero")
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			// pipe from peer to tunnel
			err = tcpConn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] Error: set write deadline failed: %v", err)
				errChan <- err
				return
			}
			packet := newDatagramPacket(buf[:n])
			if err = packet.Write(tcpConn); err != nil {
				plog.G(ctx).Errorf("[TUN-UDP] Error: %s <- %s : %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr(), err)
				errChan <- err
				return
			}
			plog.G(ctx).Infof("[TUN-UDP] %s <<< %s length: %d", tcpConn.RemoteAddr(), tcpConn.LocalAddr(), len(packet.Data))
		}
	}()
	err := <-errChan
	if err != nil {
		plog.G(ctx).Errorf("[TUN-UDP] %v", err)
	}
	plog.G(ctx).Infof("[TUN-UDP] %s >-< %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	return
}
