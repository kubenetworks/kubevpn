package core

import (
	"kubevpn/tun"
	"kubevpn/util"
	"net"
)

// tcpTransporter is a raw TCP transporter.
type tcpTransporter struct{}

// TCPTransporter creates a raw TCP client.
func TCPTransporter() Transporter {
	return &tcpTransporter{}
}

func (tr *tcpTransporter) Dial(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, util.DialTimeout)
}

type tcpListener struct {
	net.Listener
}

// TCPListener creates a Listener for TCP proxy server.
func TCPListener(addr string) (tun.Listener, error) {
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpListener{Listener: tcpKeepAliveListener{ln}}, nil
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(util.KeepAliveTime)
	return tc, nil
}
