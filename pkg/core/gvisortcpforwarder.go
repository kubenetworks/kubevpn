package core

import (
	"bytes"
	"context"
	"encoding/binary"
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

func TCPForwarder(ctx context.Context, s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return tcp.NewForwarder(s, 0, 100000, func(request *tcp.ForwarderRequest) {
		defer request.Complete(false)
		id := request.ID()
		plog.G(ctx).Infof("[TUN-TCP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)

		// 2, dial proxy
		host := id.LocalAddress.String()
		port := fmt.Sprintf("%d", id.LocalPort)
		var remote net.Conn
		var d = net.Dialer{Timeout: time.Second * 5}
		remote, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			plog.G(ctx).Errorf("[TUN-TCP] Failed to connect addr %s: %v", net.JoinHostPort(host, port), err)
			return
		}

		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			plog.G(ctx).Errorf("[TUN-TCP] Failed to create endpoint: %v", tErr)
			return
		}
		conn := gonet.NewTCPConn(w, endpoint)

		defer conn.Close()
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

func WriteProxyInfo(conn net.Conn, id stack.TransportEndpointID) error {
	var b bytes.Buffer
	i := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(i[:])
	// local port
	binary.BigEndian.PutUint16(i, id.LocalPort)
	b.Write(i)

	// remote port
	binary.BigEndian.PutUint16(i, id.RemotePort)
	b.Write(i)

	// local address
	b.WriteByte(byte(id.LocalAddress.Len()))
	b.Write(id.LocalAddress.AsSlice())

	// remote address
	b.WriteByte(byte(id.RemoteAddress.Len()))
	b.Write(id.RemoteAddress.AsSlice())
	_, err := b.WriteTo(conn)
	return err
}

// ParseProxyInfo parse proxy info [20]byte
func ParseProxyInfo(conn net.Conn) (id stack.TransportEndpointID, err error) {
	var n int
	var port = make([]byte, 2)

	// local port
	if n, err = io.ReadFull(conn, port); err != nil || n != 2 {
		return
	}
	id.LocalPort = binary.BigEndian.Uint16(port)

	// remote port
	if n, err = io.ReadFull(conn, port); err != nil || n != 2 {
		return
	}
	id.RemotePort = binary.BigEndian.Uint16(port)

	// local address
	if n, err = io.ReadFull(conn, port[:1]); err != nil || n != 1 {
		return
	}
	var localAddress = make([]byte, port[0])
	if n, err = io.ReadFull(conn, localAddress); err != nil || n != len(localAddress) {
		return
	}
	id.LocalAddress = tcpip.AddrFromSlice(localAddress)

	// remote address
	if n, err = io.ReadFull(conn, port[:1]); err != nil || n != 1 {
		return
	}
	var remoteAddress = make([]byte, port[0])
	if n, err = io.ReadFull(conn, remoteAddress); err != nil || n != len(remoteAddress) {
		return
	}
	id.RemoteAddress = tcpip.AddrFromSlice(remoteAddress)
	return
}
