package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

type tcpAddrResolver func(id stack.TransportEndpointID) (host, port string)

func serverTCPAddr(id stack.TransportEndpointID) (string, string) {
	return id.LocalAddress.String(), fmt.Sprintf("%d", id.LocalPort)
}

func localTCPAddr(id stack.TransportEndpointID) (string, string) {
	host := "127.0.0.1"
	if id.LocalAddress.To4() == (tcpip.Address{}) {
		host = net.IPv6loopback.String()
	}
	return host, fmt.Sprintf("%d", id.LocalPort)
}

// TCPForwarder creates a gvisor transport handler for TCP connections that dials the real destination.
func TCPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return newTCPForwarder(ctx, s, serverTCPAddr, false)
}

// LocalTCPForwarder creates a TCP handler that routes to 127.0.0.1 (for local dev and test mode).
func LocalTCPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return newTCPForwarder(ctx, s, localTCPAddr, true)
}

func newTCPForwarder(ctx context.Context, s *stack.Stack, resolve tcpAddrResolver, local bool) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return tcp.NewForwarder(s, 0, 100000, func(request *tcp.ForwarderRequest) {
		dialCtx := ctx
		if local {
			dialCtx = context.Background()
		}
		id := request.ID()
		plog.G(dialCtx).Infof("[Gvisor-TCP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress: %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(dialCtx).Errorf("[Gvisor-TCP] Failed to create endpoint: %v", tErr)
			request.Complete(true)
			return
		}
		if local {
			defer endpoint.Close()
		}
		conn := gonet.NewTCPConn(w, endpoint)
		defer conn.Close()
		var err error
		defer func() {
			if err != nil && !errors.Is(err, io.EOF) {
				request.Complete(true)
			} else {
				request.Complete(false)
			}
		}()

		host, port := resolve(id)
		d := net.Dialer{Timeout: config.ConnectTimeout}
		remote, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			plog.G(dialCtx).Errorf("[Gvisor-TCP] Failed to connect addr %s: %v", net.JoinHostPort(host, port), err)
			return
		}

		defer remote.Close()
		errChan := make(chan error, 2)
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(remote, conn, buf)
			plog.G(dialCtx).Infof("[Gvisor-TCP] Wrote %d bytes to remote", written)
			errChan <- err2
		}()
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(conn, remote, buf)
			plog.G(dialCtx).Infof("[Gvisor-TCP] Read %d bytes from remote", written)
			errChan <- err2
		}()
		err = <-errChan
		conn.SetReadDeadline(time.Now().Add(time.Second))
		remote.SetReadDeadline(time.Now().Add(time.Second))
		<-errChan
		if err != nil && !errors.Is(err, io.EOF) {
			plog.G(dialCtx).Errorf("[Gvisor-TCP] Disconnected %s <-> %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
		}
	}).HandlePacket
}
