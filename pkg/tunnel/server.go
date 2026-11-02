package tunnel

import (
	"context"
	"net"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel/internal/netstack"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// stackConstructor is the strategy for creating a gvisor network stack from a TUN
// link endpoint. Two implementations exist: netstack.NewStack (forwards to real destinations)
// and netstack.NewLocalStack (forwards to 127.0.0.1, for testing/dev).
//
// It is deliberately a function type, not an interface. The strategy is stateless,
// takes no construction parameters beyond (ctx, link endpoint), returns no error
// (gvisor stacks are built in-memory and cannot fail), and the returned *stack.Stack
// is a concrete gvisor type the handler must call directly — so an interface would
// not decouple callers from gvisor and would only add boilerplate. A function type
// is the idiomatic Go shape for a stateless single-method strategy. If a future stack
// implementation needs state, multiple return values (e.g. a cleanup func), or an
// error, promote THIS to an interface at that point (the single call site at
// getOrCreateClientStack is the only consumer).
type stackConstructor func(ctx context.Context, tun stack.LinkEndpoint) *stack.Stack

// gvisorTCPHandler only consumes connections; its registry belongs to the router service.
type gvisorTCPHandler struct{ *peerRegistry }

func GvisorTCPHandler(hub *Router) Handler      { return newTCPHandler(hub, netstack.NewStack) }
func GvisorLocalTCPHandler(hub *Router) Handler { return newTCPHandler(hub, netstack.NewLocalStack) }
func newTCPHandler(hub *Router, constructor stackConstructor) *gvisorTCPHandler {
	if hub == nil {
		hub = NewRouter()
	}
	hub.mu.Lock()
	// A router has exactly one registry. Additional listeners share its strategy.
	if hub.peers.newStack == nil {
		hub.peers.newStack = constructor
	}
	hub.mu.Unlock()
	return &gvisorTCPHandler{peerRegistry: hub.peers}
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	h.mu.Lock()
	diagnostics := h.diagnostics
	h.nextConnID++
	localConnID := h.nextConnID
	h.mu.Unlock()
	if diagnostics != nil {
		ctx = withDatapathDiagnostics(ctx, diagnostics)
	}
	ctx = plog.WithField(ctx, "server_conn", localConnID)
	connCtx, cancel := context.WithCancel(ctx)
	conn := newBufferedTCP(connCtx, tcpConn, func(c *bufferedTCP) {
		h.hub.RemoveRoutesByConn(connCtx, c)
	})
	defer func() {
		cancel()
		_ = conn.Close()
		conn.waitRun()
	}()
	plog.G(connCtx).Debugf("[Gvisor-TCP] Client connected: %s", tcpConn.RemoteAddr())
	h.readFromTCPConnWriteToEndpoint(connCtx, conn)
	plog.G(connCtx).Debugf("[Gvisor-TCP] Client disconnected: %s", tcpConn.RemoteAddr())
}

// handleControlConn serves a dedicated control-plane connection. It does NOT register the conn
// in Router (no AddRoute), so it never receives data-plane traffic. It reads heartbeat ICMP
// echoes from the client and injects them into the client's gvisor stack. Stack
// output routes echo replies through registered data connections, not this control conn.
func (h *gvisorTCPHandler) handleControlConn(ctx context.Context, tcpConn net.Conn) {
	inject := diagnosticStage(ctx, "gvisor_inject", "server_control", -1, 0)
	defer inject.close()
	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: tcpConn.SetReadDeadline}

	for ctx.Err() == nil {
		buf := config.LPool.Get().([]byte)[:]
		if err := dl.refresh(); err != nil {
			config.LPool.Put(buf[:])
			return
		}
		read, err := tcpConn.Read(buf[datagramHeaderLen:])
		if err != nil {
			config.LPool.Put(buf[:])
			return
		}
		if read == 0 {
			config.LPool.Put(buf[:])
			continue
		}
		ip := buf[tunReserve : datagramHeaderLen+read]
		src, _, _, parseErr := netutil.ParseIPFast(ip)
		if parseErr != nil {
			config.LPool.Put(buf[:])
			continue
		}

		inject.setPeer(src)
		// Inject heartbeat ICMP into the client's per-client gvisor stack.
		// The stack's ICMP forwarder generates a reply, which readFromEndpointWriteToRoute
		// routes back to the client via Router (through data conns, not this control conn).
		cs := h.getOrCreateClientStack(string(src))
		if cs == nil {
			config.LPool.Put(buf[:])
			return
		}
		protocol := header.IPv4ProtocolNumber
		if netutil.IsIPv6(ip) {
			protocol = header.IPv6ProtocolNumber
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(ip),
		})
		config.LPool.Put(buf[:])
		token := inject.begin()
		cs.endpoint.InjectInbound(protocol, pkt)
		inject.finish(token, 1, read, ctx.Err() == nil)
		pkt.DecRef()
	}
}
