package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

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

	id, err := util.ParseProxyInfo(tcpConn)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			plog.G(ctx).Debugf("[UDP] Connection closed: %v", err)
		} else {
			plog.G(ctx).Errorf("[UDP] Failed to parse proxy info: %v", err)
		}
		return
	}
	plog.G(ctx).Infof("[UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress: %s",
		id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
	)

	network := "udp6"
	if id.LocalAddress.Len() == 4 {
		network = "udp4"
	}
	addr := &net.UDPAddr{
		IP:   id.LocalAddress.AsSlice(),
		Port: int(id.LocalPort),
	}
	remote, err := net.DialUDP(network, nil, addr)
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errChan := make(chan error, 2)

	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)
		defer config.LPool.Put(buf)
		errChan <- pipeUDPFromTCP(ctx, tcpConn, udpConn, buf)
	}()

	go func() {
		defer util.HandleCrash()
		buf := config.LPool.Get().([]byte)
		defer config.LPool.Put(buf)
		errChan <- pipeUDPToTCP(ctx, udpConn, tcpConn, buf)
	}()

	select {
	case err := <-errChan:
		if err != nil && !errors.Is(err, io.EOF) {
			plog.G(ctx).Errorf("[UDP] Relay error: %v", err)
		}
	case <-ctx.Done():
	}
	cancel()
	plog.G(ctx).Debugf("[UDP] Relay closed: %s <-> %s", tcpConn.RemoteAddr(), udpConn.LocalAddr())
}

func pipeUDPFromTCP(ctx context.Context, tcpConn net.Conn, udpConn *net.UDPConn, buf []byte) error {
	for ctx.Err() == nil {
		if err := tcpConn.SetReadDeadline(time.Now().Add(config.UDPRelayTimeout)); err != nil {
			return fmt.Errorf("set TCP read deadline: %w", err)
		}
		datagram, err := readDatagramPacket(tcpConn, buf)
		if err != nil {
			return fmt.Errorf("read datagram: %w", err)
		}
		if datagram.DataLength == 0 {
			return errors.New("received zero-length datagram")
		}

		if err = udpConn.SetWriteDeadline(time.Now().Add(config.UDPRelayTimeout)); err != nil {
			return fmt.Errorf("set UDP write deadline: %w", err)
		}
		if _, err = udpConn.Write(datagram.Data[:datagram.DataLength]); err != nil {
			return fmt.Errorf("write UDP: %w", err)
		}
	}
	return ctx.Err()
}

func pipeUDPToTCP(ctx context.Context, udpConn *net.UDPConn, tcpConn net.Conn, buf []byte) error {
	for ctx.Err() == nil {
		if err := udpConn.SetReadDeadline(time.Now().Add(config.UDPRelayTimeout)); err != nil {
			return fmt.Errorf("set UDP read deadline: %w", err)
		}
		n, _, err := udpConn.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("read UDP: %w", err)
		}
		if n == 0 {
			return errors.New("received zero-length packet")
		}

		if err = tcpConn.SetWriteDeadline(time.Now().Add(config.UDPRelayTimeout)); err != nil {
			return fmt.Errorf("set TCP write deadline: %w", err)
		}
		if err = newDatagramPacket(buf, n).Write(tcpConn); err != nil {
			return fmt.Errorf("write TCP datagram: %w", err)
		}
	}
	return ctx.Err()
}
