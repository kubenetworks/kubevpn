package util

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestTimeout(t *testing.T) {
	udpAddr, err := net.ResolveUDPAddr("udp", "localhost:33333")
	if err != nil {
		t.Fatal(err)
	}
	var remote *net.UDPConn
	remote, err = net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	udpC := NewReadWriter(remote, time.Second*1)
	_, err = io.Copy(udpC, udpC)
	if err != nil {
		t.Fatal(err)
	}
}
