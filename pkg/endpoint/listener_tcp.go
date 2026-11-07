package endpoint

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// GvisorTCPListener creates a TLS TCP listener for the gvisor server. The data
// plane never falls back to raw, plaintext TCP: without TLS material the
// listener refuses to start.
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
			plog.G(context.Background()).Errorf("[Gvisor-TCP] TLS config not found — refusing to listen; provide %s/%s/%s",
				config.TLSCertKey, config.TLSPrivateKeyKey, config.TLSServerName)
		} else {
			plog.G(context.Background()).Errorf("[Gvisor-TCP] Failed to get TLS server config: %v", err)
		}
		_ = listener.Close()
		return nil, err
	}
	plog.G(context.Background()).Debugf("[Gvisor-TCP] Using TLS mode")
	// Derive stream obfuscation from the same secret/env source so server and
	// client agree; the tunnel handler checks the flag when wrapping each conn.
	framing.SetObfuscation(netutil.ObfuscationWanted(nil))
	return tls.NewListener(&tcpKeepAliveListener{TCPListener: listener}, serverConfig), nil
}
