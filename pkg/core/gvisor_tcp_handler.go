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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

type stackConstructor func(ctx context.Context, tun stack.LinkEndpoint) *stack.Stack

type gvisorTCPHandler struct {
	hub      *RouteHub
	newStack stackConstructor
}

// GvisorTCPHandler creates a Handler that accepts TCP connections and runs a gvisor stack per connection.
func GvisorTCPHandler(hub *RouteHub) Handler {
	return &gvisorTCPHandler{hub: hub, newStack: NewStack}
}

// GvisorLocalTCPHandler creates a Handler using NewLocalStack (routes to 127.0.0.1).
func GvisorLocalTCPHandler(hub *RouteHub) Handler {
	return &gvisorTCPHandler{hub: hub, newStack: NewLocalStack}
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	plog.G(ctx).Infof("[Gvisor-TCP] Client connected: %s", tcpConn.RemoteAddr())
	h.handle(ctx, tcpConn)
	plog.G(ctx).Infof("[Gvisor-TCP] Client disconnected: %s", tcpConn.RemoteAddr())
}

func (h *gvisorTCPHandler) handle(ctx context.Context, tcpConn net.Conn) {
	endpoint := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	// for support ipv6 skip checksum
	// vendor/gvisor.dev/gvisor/pkg/tcpip/stack/nic.go:763
	endpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	// Enable GVisor (software) GSO so the TCP layer can build large segments and gvisor
	// segments them internally before they reach this endpoint — the read side then still
	// hands normal <=MTU packets to the tunnel. We deliberately do NOT use HostGSOSupported:
	// this endpoint feeds a TCP tunnel, not a host NIC, so host-GSO would push up-to-32KB
	// super-segments onto the wire that macOS/Windows clients (MTU-sized TUN, no offload)
	// cannot write. GVisorGSO complements the TUN-side GRO (see docs/22-tun-device.md §4.2).
	endpoint.SupportedGSOKind = stack.GVisorGSOSupported
	done := make(chan struct{}, 2)
	go func() {
		defer netutil.HandleCrash()
		h.readFromTCPConnWriteToEndpoint(ctx, NewBufferedTCP(ctx, tcpConn), endpoint)
		netutil.SafeClose(done)
	}()
	go func() {
		defer netutil.HandleCrash()
		h.readFromEndpointWriteToTCPConn(ctx, tcpConn, endpoint)
		netutil.SafeClose(done)
	}()
	s := h.newStack(ctx, sniffer.NewWithPrefix(endpoint, "[gVISOR] "))
	defer s.Destroy()
	select {
	case <-done:
		return
	case <-ctx.Done():
		return
	}
}

// GvisorTCPListener creates a TCP listener with optional TLS for the gvisor server.
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
