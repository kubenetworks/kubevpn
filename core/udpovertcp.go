package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"strconv"
	"strings"
)

const (
	AddrIPv4   uint8 = 1
	AddrIPv6         = 2
	AddrDomain       = 3
)

type DatagramPacket struct {
	Type       uint8  // [1]byte
	Host       string // [?]byte, first byte is length if it's a domain
	Port       uint16 // [2]byte
	DataLength uint16 // [2]byte
	Data       []byte // []byte
}

func (addr *DatagramPacket) Length() (n int) {
	switch addr.Type {
	case AddrIPv4:
		n = 10
	case AddrIPv6:
		n = 22
	case AddrDomain:
		n = 7 + len(addr.Host)
	default:
		n = 10
	}
	return
}

func (addr *DatagramPacket) String() string {
	if addr == nil {
		return ""
	}
	return fmt.Sprintf("Type: %d, Host: %s, Port: %d, DataLength: %d, Data: %v\n",
		addr.Type, addr.Host, addr.Port, addr.DataLength, addr.Data)
}

func NewDatagramPacket(addr net.Addr, data []byte) (r *DatagramPacket) {
	defer func() {
		log.Infof("addr: %s, data: %v, result: %s", addr.String(), data, r.String())
	}()
	s := addr.String()
	var t uint8
	if strings.Count(s, ":") >= 2 {
		t = AddrIPv6
	} else {
		if ip := net.ParseIP(strings.Split(s, ":")[0]); ip != nil {
			t = AddrIPv4
		} else {
			t = AddrDomain
		}
	}
	host, port, _ := net.SplitHostPort(s)
	atoi, _ := strconv.Atoi(port)
	// todo if host is a domain
	r = &DatagramPacket{
		Host:       host,
		Port:       uint16(atoi),
		Type:       t,
		DataLength: uint16(len(data)),
		Data:       data,
	}
	return r
}

func (addr *DatagramPacket) Addr() string {
	return net.JoinHostPort(addr.Host, strconv.Itoa(int(addr.Port)))
}

func ReadDatagramPacket(r io.Reader) (rr *DatagramPacket, errsss error) {
	defer func() {
		log.Infof("result: %s", rr.String())
	}()
	//b := util.LPool.Get().([]byte)
	//defer util.LPool.Put(b)
	oneB := make([]byte, 1)

	_, err := io.ReadFull(r, oneB)
	if err != nil {
		log.Info(err)
		return nil, err
	}

	atype := oneB[0]
	d := &DatagramPacket{Type: atype}
	hostLength := 0
	switch atype {
	case AddrIPv4:
		hostLength = net.IPv4len
	case AddrIPv6:
		hostLength = net.IPv6len
	case AddrDomain:
		_, err = io.ReadFull(r, oneB)
		if err != nil {
			return nil, err
		}
		hostLength = int(oneB[0])
	default:
		return nil, errors.New("")
	}

	hostB := make([]byte, hostLength)

	if _, err = io.ReadFull(r, hostB); err != nil {
		return nil, err
	}
	var host string
	switch atype {
	case AddrIPv4:
		host = net.IPv4(hostB[0], hostB[1], hostB[2], hostB[3]).String()
	case AddrIPv6:
		p := make(net.IP, net.IPv6len)
		copy(p, hostB)
		host = p.String()
	case AddrDomain:
		host = string(hostB)
	}
	d.Host = host

	fourB := make([]byte, 4)
	if _, err = io.ReadFull(r, fourB); err != nil {
		return nil, err
	}
	d.Port = binary.BigEndian.Uint16(fourB[:2])
	d.DataLength = binary.BigEndian.Uint16(fourB[2:])

	data := make([]byte, d.DataLength)
	if _, err = io.ReadFull(r, data); err != nil && (err != io.ErrUnexpectedEOF || err != io.EOF) {
		return nil, err
	}
	d.Data = data
	rr = d
	return d, nil
}

func (addr *DatagramPacket) Write(w io.Writer) error {
	buf := bytes.Buffer{}
	buf.WriteByte(addr.Type)
	switch addr.Type {
	case AddrIPv4:
		buf.Write(net.ParseIP(addr.Host).To4())
	case AddrIPv6:
		buf.Write(net.ParseIP(addr.Host).To16())
	case AddrDomain:
		buf.WriteByte(byte(len(addr.Host)))
		buf.WriteString(addr.Host)
	}
	i := make([]byte, 2)
	binary.BigEndian.PutUint16(i, addr.Port)
	buf.Write(i)
	binary.BigEndian.PutUint16(i, uint16(len(addr.Data)))
	buf.Write(i)
	if _, err := buf.Write(addr.Data); err != nil {
		return err
	}
	_, err := buf.WriteTo(w)
	return err
}
