package util

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func WriteProxyInfo(conn net.Conn, id stack.TransportEndpointID) error {
	var b bytes.Buffer
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])
	// local port
	binary.BigEndian.PutUint16(buf, id.LocalPort)
	b.Write(buf[:2])

	// remote port
	binary.BigEndian.PutUint16(buf, id.RemotePort)
	b.Write(buf[:2])

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
