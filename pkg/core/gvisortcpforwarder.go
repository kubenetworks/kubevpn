package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/pkg/errors"
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

func TCPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return newTCPForwarder(ctx, s, serverTCPAddr, false)
}

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
		plog.G(dialCtx).Infof("[TUN-TCP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(dialCtx).Errorf("[TUN-TCP] Failed to create endpoint: %v", tErr)
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
		var d = net.Dialer{Timeout: time.Second * 5}
		var remote net.Conn
		remote, err = d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			plog.G(dialCtx).Errorf("[TUN-TCP] Failed to connect addr %s: %v", net.JoinHostPort(host, port), err)
			return
		}

		defer remote.Close()
		errChan := make(chan error, 2)
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(remote, conn, buf)
			plog.G(dialCtx).Infof("[TUN-TCP] Write length %d data to remote", written)
			errChan <- err2
		}()
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(conn, remote, buf)
			plog.G(dialCtx).Infof("[TUN-TCP] Read length %d data from remote", written)
			errChan <- err2
		}()
		err = <-errChan
		if err != nil && !errors.Is(err, io.EOF) {
			plog.G(dialCtx).Errorf("[TUN-TCP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
		}
	}).HandlePacket
}
