package core

import (
	"bytes"
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
)

func TCPForwarder(s *stack.Stack) func(stack.TransportEndpointID, *stack.PacketBuffer) bool {
	rcvWnd := 1000 << 20 // 1000MB
	return tcp.NewForwarder(s, rcvWnd, 100000, func(request *tcp.ForwarderRequest) {
		defer request.Complete(false)
		id := request.ID()
		log.Debugf("[TUN-TCP] Info: LocalPort: %d, LocalAddress: %s, RemotePort: %d, RemoteAddress %s",
			id.LocalPort, id.LocalAddress.String(), id.RemotePort, id.RemoteAddress.String(),
		)
		remote, err := net.Dial("tcp", "localhost:10801")
		if err != nil {
			log.Warningln(err)
			return
		}
		if err = WriteProxyInfo(remote, id); err != nil {
			log.Warningln(err)
			return
		}

		w := &waiter.Queue{}
		endpoint, t := request.CreateEndpoint(w)
		if t != nil {
			log.Warningln(t)
			return
		}
		conn := gonet.NewTCPConn(w, endpoint)
		go io.Copy(remote, conn)
		io.Copy(conn, remote)
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
