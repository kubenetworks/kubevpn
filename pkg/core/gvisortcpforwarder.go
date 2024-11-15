package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TCPForwarder(s *stack.Stack, ctx context.Context) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return tcp.NewForwarder(s, 0, 100000, func(request *tcp.ForwarderRequest) {
		defer request.Complete(false)
		id := request.ID()
		log.Debugf("[TUN-TCP] LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)

		// 2, dial proxy
		host := id.LocalAddress.String()
		port := fmt.Sprintf("%d", id.LocalPort)
		var remote net.Conn
		var d = net.Dialer{Timeout: time.Second * 5}
		remote, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			log.Errorf("[TUN-TCP] Failed to connect addr %s: %v", net.JoinHostPort(host, port), err)
			return
		}

		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			log.Debugf("[TUN-TCP] Failed to create endpoint: %v", tErr)
			return
		}
		conn := gonet.NewTCPConn(w, endpoint)

		defer conn.Close()
		defer remote.Close()
		errChan := make(chan error, 2)
		go func() {
			i := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(i[:])
			written, err2 := io.CopyBuffer(remote, conn, i)
			log.Debugf("[TUN-TCP] Write length %d data to remote", written)
			errChan <- err2
		}()
		go func() {
			i := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(i[:])
			written, err2 := io.CopyBuffer(conn, remote, i)
			log.Debugf("[TUN-TCP] Read length %d data from remote", written)
			errChan <- err2
		}()
		err = <-errChan
		if err != nil && !errors.Is(err, io.EOF) {
			log.Debugf("[TUN-TCP] Disconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
		}
	}).HandlePacket
}

func WriteProxyInfo(conn net.Conn, id stack.TransportEndpointID) error {
	var b bytes.Buffer
	i := config.SPool.Get().([]byte)[:]
	defer config.SPool.Put(i[:])
	binary.BigEndian.PutUint16(i, id.LocalPort)
	b.Write(i)
	binary.BigEndian.PutUint16(i, id.RemotePort)
	b.Write(i)
	b.WriteByte(byte(id.LocalAddress.Len()))
	b.Write(id.LocalAddress.AsSlice())
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
