package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type gvisorUDPHandler struct{}

// GvisorUDPHandler creates a Handler for UDP relay over TCP connections.
func GvisorUDPHandler() Handler {
	return &gvisorUDPHandler{}
}

func (h *gvisorUDPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	plog.G(ctx).Debugf("[UDP] New connection: %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	// 1, get proxy info
	id, err := util.ParseProxyInfo(tcpConn)
	if err != nil {
		plog.G(ctx).Errorf("[UDP] Failed to parse proxy info: %v", err)
		return
	}
	plog.G(ctx).Infof("[UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress: %s",
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
		plog.G(ctx).Errorf("[UDP] Failed to connect %s: %v", addr, err)
		return
	}
	relayUDPOverTCP(ctx, tcpConn, remote)
}

// GvisorUDPListener creates a TCP listener for UDP-over-TCP relay connections.
func GvisorUDPListener(addr string) (net.Listener, error) {
	plog.G(context.Background()).Infof("[UDP] Listening on %s", addr)
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

func relayUDPOverTCP(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn) {
	defer udpConn.Close()
	plog.G(ctx).Debugf("[UDP] Relaying: %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
	errChan := make(chan error, 2)
	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])

		for ctx.Err() == nil {
			err := tcpConn.SetReadDeadline(time.Now().Add(config.UDPRelayTimeout))
			if err != nil {
				errChan <- fmt.Errorf("set read deadline failed: %w", err)
				return
			}
			datagram, err := readDatagramPacket(tcpConn, buf)
			if err != nil {
				errChan <- fmt.Errorf("read datagram packet failed: %w", err)
				return
			}
			if datagram.DataLength == 0 {
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			err = udpConn.SetWriteDeadline(time.Now().Add(config.UDPRelayTimeout))
			if err != nil {
				errChan <- fmt.Errorf("set write deadline failed: %w", err)
				return
			}
			if _, err = udpConn.Write(datagram.Data[:datagram.DataLength]); err != nil {
				errChan <- fmt.Errorf("write datagram packet failed: %w", err)
				return
			}
			plog.G(ctx).Debugf("[UDP] Sent %d bytes: %s -> %s", datagram.DataLength, tcpConn.RemoteAddr(), udpConn.RemoteAddr())
		}
	}()

	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)[:]
		defer config.LPool.Put(buf[:])

		for ctx.Err() == nil {
			err := udpConn.SetReadDeadline(time.Now().Add(config.UDPRelayTimeout))
			if err != nil {
				errChan <- fmt.Errorf("set read deadline failed: %w", err)
				return
			}
			n, _, err := udpConn.ReadFrom(buf[:])
			if err != nil {
				errChan <- fmt.Errorf("read datagram packet failed: %w", err)
				return
			}
			if n == 0 {
				errChan <- fmt.Errorf("length of read packet is zero")
				return
			}

			// pipe from peer to tunnel
			err = tcpConn.SetWriteDeadline(time.Now().Add(config.UDPRelayTimeout))
			if err != nil {
				errChan <- fmt.Errorf("set write deadline failed: %w", err)
				return
			}
			packet := newDatagramPacket(buf, n)
			if err = packet.Write(tcpConn); err != nil {
				errChan <- err
				return
			}
			plog.G(ctx).Debugf("[UDP] Received %d bytes: %s <- %s", packet.DataLength, tcpConn.RemoteAddr(), tcpConn.LocalAddr())
		}
	}()
	err := <-errChan
	if err != nil && !errors.Is(err, io.EOF) {
		plog.G(ctx).Errorf("[UDP] Relay error: %v", err)
	}
	plog.G(ctx).Debugf("[UDP] Relay closed: %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
}
