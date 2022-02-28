package core

import (
	"crypto/tls"
	"github.com/wencaiwulue/kubevpn/config"
	"net"
)

// tcpTransporter is a raw TCP transporter.
type tcpTransporter struct{}

// TCPTransporter creates a raw TCP client.
func TCPTransporter() Transporter {
	return &tcpTransporter{}
}

func (tr *tcpTransporter) Dial(addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: config.DialTimeout}
	return tls.DialWithDialer(dialer, "tcp", addr, config.TlsConfigClient)
}

type tcpListener struct {
	net.Listener
}

// TCPListener creates a Listener for TCP proxy server.
func TCPListener(addr string) (net.Listener, error) {
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpListener{Listener: tcpKeepAliveListener{TCPListener: ln}}, nil
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(config.KeepAliveTime)
	return tc, nil
}
