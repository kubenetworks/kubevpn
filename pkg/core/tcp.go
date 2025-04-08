package core

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type tcpTransporter struct {
	tlsConfig *tls.Config
}

func TCPTransporter(tlsInfo map[string][]byte) Transporter {
	tlsConfig, err := util.GetTlsClientConfig(tlsInfo)
	if err != nil {
		if errors.Is(err, util.ErrNoTLSConfig) {
			plog.G(context.Background()).Warn("tls config not found in config, use raw tcp mode")
			return &tcpTransporter{}
		}
		plog.G(context.Background()).Errorf("failed to get tls client config: %v", err)
		return &tcpTransporter{}
	}
	return &tcpTransporter{tlsConfig: tlsConfig}
}

func (tr *tcpTransporter) Dial(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: config.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if tr.tlsConfig == nil {
		plog.G(ctx).Debugf("tls config not found in config, use raw tcp mode")
		return conn, nil
	}
	plog.G(ctx).Debugf("use tls mode")
	return tls.Client(conn, tr.tlsConfig), nil
}

func TCPListener(addr string) (net.Listener, error) {
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	serverConfig, err := util.GetTlsServerConfig(nil)
	if err != nil {
		if errors.Is(err, util.ErrNoTLSConfig) {
			plog.G(context.Background()).Warn("tls config not found in config, use raw tcp mode")
			return &tcpKeepAliveListener{TCPListener: listener}, nil
		}
		plog.G(context.Background()).Errorf("failed to get tls server config: %v", err)
		return nil, err
	}
	return tls.NewListener(&tcpKeepAliveListener{TCPListener: listener}, serverConfig), nil
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
