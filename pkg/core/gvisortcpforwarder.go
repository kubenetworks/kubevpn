package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"

	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

var GvisorTCPForwardAddr string

func TCPForwarder(s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	return tcp.NewForwarder(s, 0, 100000, func(request *tcp.ForwarderRequest) {
		defer request.Complete(false)
		id := request.ID()
		log.Debugf("[TUN-TCP] Debug: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)

		node, err := ParseNode(GvisorTCPForwardAddr)
		if err != nil {
			log.Debugf("[TUN-TCP] Error: can not parse gvisor tcp forward addr %s: %v", GvisorTCPForwardAddr, err)
			return
		}
		node.Client = &Client{
			Connector:   GvisorTCPTunnelConnector(),
			Transporter: TCPTransporter(),
		}
		forwardChain := NewChain(5, node)

		remote, err := forwardChain.dial(context.Background())
		if err != nil {
			log.Debugf("[TUN-TCP] Error: failed to dial remote conn: %v", err)
			return
		}
		if err = WriteProxyInfo(remote, id); err != nil {
			log.Debugf("[TUN-TCP] Error: failed to write proxy info: %v", err)
			return
		}

		w := &waiter.Queue{}
		endpoint, tErr := request.CreateEndpoint(w)
		if tErr != nil {
			log.Debugf("[TUN-TCP] Error: can not create endpoint: %v", tErr)
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
			log.Debugf("[TUN-TCP] Debug: write length %d data to remote", written)
			errChan <- err2
		}()
		go func() {
			i := config.LPool.Get().([]byte)[:]
			defer config.LPool.Put(i[:])
			written, err2 := io.CopyBuffer(conn, remote, i)
			log.Debugf("[TUN-TCP] Debug: read length %d data from remote", written)
			errChan <- err2
		}()
		err = <-errChan
		if err != nil && !errors.Is(err, io.EOF) {
			log.Debugf("[TUN-TCP] Error: dsiconnect: %s >-<: %s: %v", conn.LocalAddr(), remote.RemoteAddr(), err)
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
