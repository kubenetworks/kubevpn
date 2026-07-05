package core

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type udpAddrResolver func(id stack.TransportEndpointID) *net.UDPAddr

func serverUDPAddr(id stack.TransportEndpointID) *net.UDPAddr {
	return &net.UDPAddr{IP: id.LocalAddress.AsSlice(), Port: int(id.LocalPort)}
}

func localUDPAddr(id stack.TransportEndpointID) *net.UDPAddr {
	ip := net.ParseIP("127.0.0.1")
	if id.LocalAddress.To4() == (tcpip.Address{}) {
		ip = net.IPv6loopback
	}
	return &net.UDPAddr{IP: ip, Port: int(id.LocalPort)}
}

// UDPForwarder creates a gvisor transport handler for UDP packets that dials the real destination.
func UDPForwarder(ctx context.Context, s *stack.Stack) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return newUDPForwarder(ctx, s, serverUDPAddr)
}

// LocalUDPForwarder creates a UDP handler that routes to 127.0.0.1 (for local dev mode).
func LocalUDPForwarder(ctx context.Context, s *stack.Stack) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return newUDPForwarder(ctx, s, localUDPAddr)
}

func newUDPForwarder(ctx context.Context, s *stack.Stack, resolve udpAddrResolver) func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	return udp.NewForwarder(s, func(request *udp.ForwarderRequest) {
		id := request.ID()
		plog.G(ctx).Infof("[Gvisor-UDP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress: %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		src := &net.UDPAddr{
			IP:   id.RemoteAddress.AsSlice(),
			Port: int(id.RemotePort),
		}
		dst := resolve(id)

		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(ctx).Errorf("[Gvisor-UDP] Failed to create endpoint to dst: %s: %v", dst.String(), tErr)
			return
		}

		remote, err := net.DialUDP("udp", nil, dst)
		if err != nil {
			plog.G(ctx).Errorf("[Gvisor-UDP] Failed to connect dst: %s: %v", dst.String(), err)
			return
		}

		conn := gonet.NewUDPConn(w, endpoint)
		go relayUDP(ctx, conn, remote, src, dst)
	}).HandlePacket
}

func relayUDP(ctx context.Context, conn *gonet.UDPConn, remote *net.UDPConn, src, dst *net.UDPAddr) {
	defer conn.Close()
	defer remote.Close()
	errChan := make(chan error, 2)
	go pipeUDPFromConn(ctx, conn, remote, src, dst, errChan)
	go pipeUDPToConn(ctx, conn, remote, src, dst, errChan)
	if err := <-errChan; err != nil && !errors.Is(err, io.EOF) {
		plog.G(ctx).Errorf("[Gvisor-UDP] Disconnected %s <-> %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
	}
}

func pipeUDPFromConn(ctx context.Context, conn *gonet.UDPConn, remote *net.UDPConn, src, dst *net.UDPAddr, errChan chan<- error) {
	defer util.HandleCrash()
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])
	var written int
	var err error
	for {
		if err = conn.SetReadDeadline(time.Now().Add(config.UDPSessionTimeout)); err != nil {
			break
		}
		var read int
		read, _, err = conn.ReadFrom(buf[:])
		if err != nil {
			break
		}
		written += read
		if err = remote.SetWriteDeadline(time.Now().Add(config.UDPSessionTimeout)); err != nil {
			break
		}
		if _, err = remote.Write(buf[:read]); err != nil {
			break
		}
	}
	plog.G(ctx).Infof("[Gvisor-UDP] Wrote %d bytes: %s -> %s", written, src, dst)
	errChan <- err
}

func pipeUDPToConn(ctx context.Context, conn *gonet.UDPConn, remote *net.UDPConn, src, dst *net.UDPAddr, errChan chan<- error) {
	defer util.HandleCrash()
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])
	var written int
	var err error
	for {
		if err = remote.SetReadDeadline(time.Now().Add(config.UDPSessionTimeout)); err != nil {
			break
		}
		var n int
		n, _, err = remote.ReadFromUDP(buf[:])
		if err != nil {
			break
		}
		written += n
		if err = conn.SetWriteDeadline(time.Now().Add(config.UDPSessionTimeout)); err != nil {
			break
		}
		if _, err = conn.Write(buf[:n]); err != nil {
			break
		}
	}
	plog.G(ctx).Infof("[Gvisor-UDP] Read %d bytes: %s <- %s", written, src, dst)
	errChan <- err
}
