package core

import (
	"encoding/binary"
	"io"
)

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
