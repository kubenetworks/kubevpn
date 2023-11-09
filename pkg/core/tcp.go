package core

import (
	"context"
	"net"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/errors"
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
		err = errors.Wrap(err, "net.ResolveTCPAddr(\"tcp\", addr): ")
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		err = errors.Wrap(err, "net.ListenTCP(\"tcp\", laddr): ")
		return nil, err
	}
	return &tcpKeepAliveListener{ln}, nil
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln *tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	conn, err := ln.AcceptTCP()
	if err != nil {
		err = errors.Wrap(err, "ln.AcceptTCP(): ")
		return
	}
	err = conn.SetKeepAlive(true)
	if err != nil {
		err = errors.Wrap(err, "conn.SetKeepAlive(true): ")
		return nil, err
	}
	err = conn.SetKeepAlivePeriod(config.KeepAliveTime)
	if err != nil {
		err = errors.Wrap(err, "conn.SetKeepAlivePeriod(config.KeepAliveTime): ")
		return nil, err
	}
	err = conn.SetNoDelay(true)
	if err != nil {
		err = errors.Wrap(err, "conn.SetNoDelay(true): ")
		return nil, err
	}
	return conn, nil
}
