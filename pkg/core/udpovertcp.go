package core

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type datagramPacket struct {
	DataLength uint16 // [2]byte
	Data       []byte // []byte
}

func (addr *datagramPacket) String() string {
	if addr == nil {
		return ""
	}
	return fmt.Sprintf("DataLength: %d, Data: %v\n", addr.DataLength, addr.Data)
}

func newDatagramPacket(data []byte) (r *datagramPacket) {
	return &datagramPacket{
		DataLength: uint16(len(data)),
		Data:       data,
	}
}

func (addr *datagramPacket) Addr() net.Addr {
	var server8422, _ = net.ResolveUDPAddr("udp", "127.0.0.1:8422")
	return server8422
}

func readDatagramPacket(r io.Reader, b []byte) (*datagramPacket, error) {
	_, err := io.ReadFull(r, b[:2])
	if err != nil {
		return nil, err
	}
	dataLength := binary.BigEndian.Uint16(b[:2])
	_, err = io.ReadFull(r, b[:dataLength])
	if err != nil /*&& (err != io.ErrUnexpectedEOF || err != io.EOF)*/ {
		return nil, err
	}
	return &datagramPacket{DataLength: dataLength, Data: b[:dataLength]}, nil
}

// this method will return all byte array in the way: b[:]
func readDatagramPacketServer(r io.Reader, b []byte) (*datagramPacket, error) {
	_, err := io.ReadFull(r, b[:2])
	if err != nil {
		return nil, err
	}
	dataLength := binary.BigEndian.Uint16(b[:2])
	_, err = io.ReadFull(r, b[:dataLength])
	if err != nil /*&& (err != io.ErrUnexpectedEOF || err != io.EOF)*/ {
		return nil, err
	}
	return &datagramPacket{DataLength: dataLength, Data: b[:]}, nil
}

func (addr *datagramPacket) Write(w io.Writer) error {
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])
	binary.BigEndian.PutUint16(buf[:2], uint16(len(addr.Data)))
	n := copy(buf[2:], addr.Data)
	_, err := w.Write(buf[:n+2])
	return err
}
