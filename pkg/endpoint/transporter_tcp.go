package endpoint

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

type tcpTransporter struct {
	tlsConfig *tls.Config
}

// TCPTransporter creates a Transporter that dials TCP connections with TLS.
// When TLS config is absent or invalid, returns a transporter that refuses
// connections: the data plane never falls back to raw, plaintext TCP.
func TCPTransporter(tlsInfo map[string][]byte) Transporter {
	tlsConfig, err := netutil.GetTlsClientConfig(tlsInfo)
	if err != nil {
		if errors.Is(err, netutil.ErrNoTLSConfig) {
			plog.G(context.Background()).Errorf("[Transport] TLS config not found — connections will fail; provide %s/%s/%s",
				config.TLSCertKey, config.TLSPrivateKeyKey, config.TLSServerName)
			return &failTransporter{err: fmt.Errorf("TLS config not found: %w", netutil.ErrNoTLSConfig)}
		}
		plog.G(context.Background()).Errorf("[Transport] TLS config invalid: %v — connections will fail", err)
		return &failTransporter{err: fmt.Errorf("TLS config invalid: %w", err)}
	}
	// Derive stream obfuscation from the same secret/env source so client and
	// server agree; the connector checks the flag when wrapping each conn.
	framing.SetObfuscation(netutil.ObfuscationWanted(tlsInfo))
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
		// TCPTransporter never builds this without a config; guard against future misuse.
		plog.G(ctx).Error("[Transport] TLS config missing, refusing raw TCP")
		conn.Close()
		return nil, fmt.Errorf("TLS config missing: %w", netutil.ErrNoTLSConfig)
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
