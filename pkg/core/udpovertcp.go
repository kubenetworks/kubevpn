package core

import (
	"encoding/binary"
	"io"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type DatagramPacket struct {
	DataLength uint16 // [2]byte
	Data       []byte // []byte
}

func newDatagramPacket(data []byte) (r *DatagramPacket) {
	return &DatagramPacket{
		DataLength: uint16(len(data)),
		Data:       data,
	}
}

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
	return &DatagramPacket{DataLength: dataLength, Data: b[:dataLength]}, nil
}

// this method will return all byte array in the way: b[:]
func readDatagramPacketServer(r io.Reader, b []byte) (*DatagramPacket, error) {
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
	buf := config.LPool.Get().([]byte)[:]
	defer config.LPool.Put(buf[:])
	binary.BigEndian.PutUint16(buf[:2], uint16(len(d.Data)))
	n := copy(buf[2:], d.Data[:d.DataLength])
	_, err := w.Write(buf[:n+2])
	return err
}
