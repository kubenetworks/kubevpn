package core

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// GvisorTCPListener creates a TCP listener with optional TLS for the gvisor server.
func GvisorTCPListener(addr string) (net.Listener, error) {
	plog.G(context.Background()).Debugf("[Gvisor-TCP] Listening on %s", addr)
	laddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenTCP("tcp", laddr)
	if err != nil {
		return nil, err
	}
	serverConfig, err := netutil.GetTlsServerConfig(nil)
	if err != nil {
		if errors.Is(err, netutil.ErrNoTLSConfig) {
			plog.G(context.Background()).Warn("[Gvisor-TCP] TLS config not found, using raw TCP")
			return &tcpKeepAliveListener{TCPListener: listener}, nil
		}
		plog.G(context.Background()).Errorf("[Gvisor-TCP] Failed to get TLS server config: %v", err)
		_ = listener.Close()
		return nil, err
	}
	plog.G(context.Background()).Debugf("[Gvisor-TCP] Using TLS mode")
	return tls.NewListener(&tcpKeepAliveListener{TCPListener: listener}, serverConfig), nil
}
