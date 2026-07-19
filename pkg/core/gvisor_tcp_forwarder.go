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

// tcpForwarderMaxInFlight bounds the number of in-flight TCP forwarder connection attempts.
// 8192 matches Tailscale's netstack limit for Linux subnet routers — high enough that real
// workloads won't hit it, without the unbounded memory exposure of the previous 100000.
const tcpForwarderMaxInFlight = 8192

func newTCPForwarder(ctx context.Context, s *stack.Stack, resolve tcpAddrResolver, local bool) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	// role distinguishes the two forwarders in the logs: "local" is the reverse path
	// (envoy/cluster -> this machine's 127.0.0.1 dev service), "cluster" is the forward
	// path (client -> real k8s destination). Tracing a 503/EOF needs to know which side.
	role := "cluster"
	if local {
		role = "local"
	}
	// rcvWnd = DefaultReceiveBufferSize: seed the forwarder's receive window with gvisor's
	// default instead of 0 (which starts at the minimum window and throttles new connections
	// until autotuning ramps up). The window still autotunes up to the range cap set in
	// newGvisorStack.
	return tcp.NewForwarder(s, tcp.DefaultReceiveBufferSize, tcpForwarderMaxInFlight, func(request *tcp.ForwarderRequest) {
		dialCtx := ctx
		if local {
			dialCtx = context.Background()
		}
		id := request.ID()
		plog.G(dialCtx).Debugf("[Gvisor-TCP][%s] new conn dst=%s:%d src=%s:%d",
			role, id.LocalAddress.String(), id.LocalPort, id.RemoteAddress.String(), id.RemotePort,
		)
		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(dialCtx).Errorf("[Gvisor-TCP][%s] Failed to create endpoint (dst=%s:%d): %v", role, id.LocalAddress.String(), id.LocalPort, tErr)
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
		target := net.JoinHostPort(host, port)
		plog.G(dialCtx).Debugf("[Gvisor-TCP][%s] dialing %s", role, target)
		d := net.Dialer{Timeout: config.ConnectTimeout}
		remote, err := d.DialContext(dialCtx, "tcp", target)
		if err != nil {
			plog.G(dialCtx).Errorf("[Gvisor-TCP][%s] Failed to connect addr %s: %v", role, target, err)
			return
		}

		defer remote.Close()
		errChan := make(chan error, 2)
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(remote, conn, buf)
			plog.G(dialCtx).Debugf("[Gvisor-TCP][%s] Wrote %d bytes to %s", role, written, target)
			errChan <- err2
		}()
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(conn, remote, buf)
			plog.G(dialCtx).Debugf("[Gvisor-TCP][%s] Read %d bytes from %s", role, written, target)
			errChan <- err2
		}()
		err = <-errChan
		conn.SetReadDeadline(time.Now().Add(time.Second))
		remote.SetReadDeadline(time.Now().Add(time.Second))
		<-errChan
		if err != nil && !errors.Is(err, io.EOF) {
			plog.G(dialCtx).Errorf("[Gvisor-TCP][%s] Disconnected %s <-> %s: %v", role, conn.LocalAddr(), remote.RemoteAddr(), err)
		}
	}).HandlePacket
}
