package core

import (
	"context"
	"encoding/binary"
	"io"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func NewUDPConnOverTCP(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return &UDPConnOverTCP{ctx: ctx, Conn: conn}, nil
}

var _ net.Conn = (*UDPConnOverTCP)(nil)

// UDPConnOverTCP fake udp connection over tcp connection
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
	buf := config.LPool.Get().([]byte)[:]
	n := copy(buf, b)
	defer config.LPool.Put(buf)

	packet := newDatagramPacket(buf, n)
	if err := packet.Write(c.Conn); err != nil {
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
