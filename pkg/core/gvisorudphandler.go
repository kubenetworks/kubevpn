package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/pkg/errors"
	"gvisor.dev/gvisor/pkg/tcpip"

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
	plog.G(ctx).Debugf("[TUN-UDP] %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	// 1, get proxy info
	id, err := util.ParseProxyInfo(tcpConn)
	if err != nil {
		plog.G(ctx).Errorf("[TUN-UDP] Failed to parse proxy info: %v", err)
		return
	}
	plog.G(ctx).Infof("[TUN-UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress: %s",
		id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
	)
	// 2, dial proxy
	addr := &net.UDPAddr{
		IP:   id.LocalAddress.AsSlice(),
		Port: int(id.LocalPort),
	}
	var network string
	if id.LocalAddress.To4() != (tcpip.Address{}) {
		network = "udp4"
	} else {
		network = "udp6"
	}
	var remote *net.UDPConn
	remote, err = net.DialUDP(network, nil, addr)
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
		datagram, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, err
		}
		return int(datagram.DataLength), nil
	}
}

func (c *gvisorUDPConnOverTCP) Write(b []byte) (int, error) {
	buf := config.LPool.Get().([]byte)[:]
	n := copy(buf, b)
	defer config.LPool.Put(buf)

	packet := newDatagramPacket(buf, n)
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
	plog.G(ctx).Debugf("[TUN-UDP] %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	errChan := make(chan error, 2)
	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])

		for ctx.Err() == nil {
			err := tcpConn.SetReadDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				errChan <- errors.WithMessage(err, "set read deadline failed")
				return
			}
			datagram, err := readDatagramPacket(tcpConn, buf)
			if err != nil {
				errChan <- errors.WithMessage(err, "read datagram packet failed")
				return
			}
			if datagram.DataLength == 0 {
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			err = udpConn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				errChan <- errors.WithMessage(err, "set write deadline failed")
				return
			}
			if _, err = udpConn.Write(datagram.Data[:datagram.DataLength]); err != nil {
				errChan <- errors.WithMessage(err, "write datagram packet failed")
				return
			}
			plog.G(ctx).Debugf("[TUN-UDP] %s >>> %s length: %d", tcpConn.RemoteAddr(), udpConn.RemoteAddr(), datagram.DataLength)
		}
	}()

	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])

		for ctx.Err() == nil {
			err := udpConn.SetReadDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				errChan <- errors.WithMessage(err, "set read deadline failed")
				return
			}
			n, _, err := udpConn.ReadFrom(buf[:])
			if err != nil {
				errChan <- errors.WithMessage(err, "read datagram packet failed")
				return
			}
			if n == 0 {
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			// pipe from peer to tunnel
			err = tcpConn.SetWriteDeadline(time.Now().Add(time.Second * 30))
			if err != nil {
				errChan <- errors.WithMessage(err, "set write deadline failed")
				return
			}
			packet := newDatagramPacket(buf, n)
			if err = packet.Write(tcpConn); err != nil {
				errChan <- err
				return
			}
			plog.G(ctx).Debugf("[TUN-UDP] %s <<< %s length: %d", tcpConn.RemoteAddr(), tcpConn.LocalAddr(), packet.DataLength)
		}
	}()
	err := <-errChan
	if err != nil && !errors.Is(err, io.EOF) {
		plog.G(ctx).Errorf("[TUN-UDP] %v", err)
	}
	plog.G(ctx).Debugf("[TUN-UDP] %s >-< %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	return
}
