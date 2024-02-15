package core

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type tcpTransporter struct{}

func TCPTransporter() Transporter {
	return &tcpTransporter{}
}

func (tr *tcpTransporter) Dial(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: config.DialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}

func TCPListener(addr string) (net.Listener, error) {
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	return &tcpKeepAliveListener{TCPListener: ln}, nil
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln *tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	conn, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	err = conn.SetKeepAlive(true)
	if err != nil {
		return nil, err
	}
	err = conn.SetKeepAlivePeriod(config.KeepAliveTime)
	if err != nil {
		return nil, err
	}
	err = conn.SetNoDelay(true)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
