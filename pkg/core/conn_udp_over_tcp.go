package core

import (
	"context"
	"encoding/binary"
	"io"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// NewUDPConnOverTCP wraps a TCP connection with datagram framing for UDP-like semantics.
func NewUDPConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &UDPConnOverTCP{ctx: ctx, Conn: conn}, nil
}

var _ net.Conn = (*UDPConnOverTCP)(nil)

// UDPConnOverTCP provides UDP-like datagram semantics over a TCP stream using length-prefix framing.
type UDPConnOverTCP struct {
	// tcp connection
	net.Conn
	ctx context.Context
}

func (c *UDPConnOverTCP) Read(b []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		datagram, err := readDatagramPacket(c.Conn, b)
		if err != nil {
			return 0, err
		}
		return int(datagram.DataLength), nil
	}
}

func (c *UDPConnOverTCP) Write(b []byte) (int, error) {
	buf := config.LPool.Get().([]byte)
	n := copy(buf[2:], b)
	binary.BigEndian.PutUint16(buf[:2], uint16(n))
	_, err := c.Conn.Write(buf[:n+2])
	config.LPool.Put(buf)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *UDPConnOverTCP) Close() error {
	if cc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		_ = cc.CloseRead()
	}
	if cc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cc.CloseWrite()
	}
	return c.Conn.Close()
}

// DatagramPacket represents a length-prefixed datagram over a stream connection.
type DatagramPacket struct {
	DataLength uint16 // [2]byte
	Data       []byte // []byte
}

func newDatagramPacket(data []byte, length int) (r *DatagramPacket) {
	return &DatagramPacket{
		DataLength: uint16(length),
		Data:       data,
	}
}

// this method will return all byte array in the way: b[:], len(DatagramPacket.Data)==64k
func readDatagramPacket(r io.Reader, b []byte) (*DatagramPacket, error) {
	_, err := io.ReadFull(r, b[:2])
	if err != nil {
		return nil, err
	}
	dataLength := binary.BigEndian.Uint16(b[:2])
	_, err = io.ReadFull(r, b[:dataLength])
	if err != nil {
		return nil, err
	}
	return &DatagramPacket{DataLength: dataLength, Data: b[:]}, nil
}

func (d *DatagramPacket) Write(w io.Writer) error {
	n := copy(d.Data[2:], d.Data[:d.DataLength])
	binary.BigEndian.PutUint16(d.Data[:2], d.DataLength)
	_, err := w.Write(d.Data[:n+2])
	return err
}
