package core

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type tcpTransporter struct {
	tlsConfig *tls.Config
}

// TCPTransporter creates a Transporter that dials TCP connections with optional TLS.
// When TLS config is absent (ErrNoTLSConfig), falls back to raw TCP.
// When TLS config exists but is invalid, returns a transporter that refuses connections.
func TCPTransporter(tlsInfo map[string][]byte) Transporter {
	tlsConfig, err := util.GetTlsClientConfig(tlsInfo)
	if err != nil {
		if errors.Is(err, util.ErrNoTLSConfig) {
			plog.G(context.Background()).Warn("[Transport] TLS config not found, using raw TCP")
			return &tcpTransporter{}
		}
		plog.G(context.Background()).Errorf("[Transport] TLS config invalid: %v — connections will fail", err)
		return &failTransporter{err: fmt.Errorf("TLS config invalid: %w", err)}
	}
	return &tcpTransporter{tlsConfig: tlsConfig}
}

type failTransporter struct{ err error }

func (t *failTransporter) Dial(_ context.Context, _ string) (net.Conn, error) {
	return nil, t.err
}

func (tr *tcpTransporter) Dial(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: config.DialTimeout, KeepAlive: config.KeepAliveTime}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if tr.tlsConfig == nil {
		plog.G(ctx).Debugf("[Transport] TLS config not found, using raw TCP")
		return conn, nil
	}
	plog.G(ctx).Debugf("[Transport] Using TLS mode")
	return tls.Client(conn, tr.tlsConfig), nil
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
