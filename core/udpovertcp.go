package core

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

type DatagramPacket struct {
	DataLength uint16 // [2]byte
	Data       []byte // []byte
}

func (addr *DatagramPacket) String() string {
	if addr == nil {
		return ""
	}
	return fmt.Sprintf("DataLength: %d, Data: %v\n", addr.DataLength, addr.Data)
}

func NewDatagramPacket(data []byte) (r *DatagramPacket) {
	return &DatagramPacket{
		DataLength: uint16(len(data)),
		Data:       data,
	}
}

func (addr *DatagramPacket) Addr() net.Addr {
	return Server8422
}

func ReadDatagramPacket(r io.Reader) (*DatagramPacket, error) {
	b := LPool.Get().([]byte)
	defer LPool.Put(b)
	_, err := io.ReadFull(r, b[:2])
	if err != nil {
		return nil, err
	}
	d := &DatagramPacket{}
	d.DataLength = binary.BigEndian.Uint16(b[:2])
	if _, err = io.ReadFull(r, b[:d.DataLength]); err != nil && (err != io.ErrUnexpectedEOF || err != io.EOF) {
		return nil, err
	}
	d.Data = b[:d.DataLength]
	return d, nil
}

func (addr *DatagramPacket) Write(w io.Writer) error {
	buf := bytes.Buffer{}
	i := make([]byte, 2)
	binary.BigEndian.PutUint16(i, uint16(len(addr.Data)))
	buf.Write(i)
	if _, err := buf.Write(addr.Data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}
