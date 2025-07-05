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

func LocalTCPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return tcp.NewForwarder(s, 0, 100000, func(request *tcp.ForwarderRequest) {
		ctx = context.Background()
		defer request.Complete(true)
		id := request.ID()
		plog.G(ctx).Infof("[TUN-TCP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(ctx).Errorf("[TUN-TCP] Failed to create endpoint: %v", tErr)
			return
		}
		defer endpoint.Close()
		conn := gonet.NewTCPConn(w, endpoint)
		defer conn.Close()

		// 2, dial proxy
		var host string
		if id.LocalAddress.To4() != (tcpip.Address{}) {
			host = "127.0.0.1"
		} else {
			host = net.IPv6loopback.String()
		}
		port := fmt.Sprintf("%d", id.LocalPort)
		var remote net.Conn
		var d = net.Dialer{Timeout: time.Second * 5}
		remote, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			plog.G(ctx).Errorf("[TUN-TCP] Failed to connect addr %s: %v", net.JoinHostPort(host, port), err)
			return
		}

		defer remote.Close()
		errChan := make(chan error, 2)
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(remote, conn, buf)
			plog.G(ctx).Infof("[TUN-TCP] Write length %d data to remote", written)
			errChan <- err2
		}()
		go func() {
			buf := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(buf[:])
			written, err2 := io.CopyBuffer(conn, remote, buf)
			plog.G(ctx).Infof("[TUN-TCP] Read length %d data from remote", written)
			errChan <- err2
		}()
		err = <-errChan
		if err != nil && !errors.Is(err, io.EOF) {
			plog.G(ctx).Errorf("[TUN-TCP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
		}
	}).HandlePacket
}
