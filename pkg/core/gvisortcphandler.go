package core

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type gvisorTCPHandler struct {
	hub *RouteHub
}

func GvisorTCPHandler(hub *RouteHub) Handler {
	if hub == nil {
		hub = DefaultRouteHub
	}
	return &gvisorTCPHandler{
		hub: hub,
	}
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	plog.G(ctx).Infof("[Gvisor-TCP] New connection: %s -> %s", tcpConn.RemoteAddr(), tcpConn.LocalAddr())
	h.handle(ctx, tcpConn)
}

func (h *gvisorTCPHandler) handle(ctx context.Context, tcpConn net.Conn) {
	endpoint := channel.New(tcp.DefaultReceiveBufferSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	// for support ipv6 skip checksum
	// vendor/gvisor.dev/gvisor/pkg/tcpip/stack/nic.go:763
	endpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	errChan := make(chan error, 2)
	go func() {
		defer util.HandleCrash()
		h.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, tcpConn), endpoint)
		util.SafeClose(errChan)
	}()
	go func() {
		defer util.HandleCrash()
		h.readFromEndpointWriteToTCPConn(ctx, tcpConn, endpoint)
		util.SafeClose(errChan)
	}()
	s := NewStack(ctx, sniffer.NewWithPrefix(endpoint, "[gVISOR] "))
	defer s.Destroy()
	select {
	case <-errChan:
		return
	case <-ctx.Done():
		return
	}
}

func GvisorTCPListener(addr string) (net.Listener, error) {
	plog.G(context.Background()).Infof("[Gvisor-TCP] Listening on %s", addr)
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
