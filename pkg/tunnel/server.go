package tunnel

import (
	"bytes"
	"context"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
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
	if framing.ObfuscationEnabled() {
		// Wrap below the framing/buffering layers: every byte the client's
		// connector obfuscated (datagrams, batches, route writes, control
		// frames) is de-obfuscated here before it reaches any reader.
		tcpConn = framing.ObfuscateAccept(tcpConn)
	}
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
// in Router (no AddRoute), so it never receives data-plane traffic. It answers heartbeat ICMP
// echo requests inline on this same conn instead of injecting them into the client's gvisor
// stack: the reply no longer depends on the client's registered data connections, so a kernel
// RTO stall on the single data slot (see docs/50) cannot make the liveness watchdog retire a
// self-recovering generation. The client's control-slot reader correlates the reply exactly as
// a data-slot one would (readGeneration → observeReply). getOrCreateClientStack keeps the old
// stack lifecycle contract: a control-only heartbeat still creates the client's stack, and
// control-only lookups still never refresh orphan grace.
func (h *gvisorTCPHandler) handleControlConn(ctx context.Context, tcpConn net.Conn) {
	inject := diagnosticStage(ctx, "gvisor_inject", "server_control", -1, 0)
	defer inject.close()
	write := diagnosticStage(ctx, "socket_write", "server_control", -1, 0)
	defer write.close()
	dl := &periodicDeadline{timeout: config.KeepAliveTime * 3, set: tcpConn.SetReadDeadline}
	writeDL := &periodicDeadline{timeout: config.KeepAliveTime, set: tcpConn.SetWriteDeadline}

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
		cs := h.getOrCreateClientStack(string(src))
		if cs == nil {
			config.LPool.Put(buf[:])
			return
		}
		// The ICMP body (ident/seq plus the heartbeat correlation payload) is echoed
		// verbatim; only addresses, type and checksums change. Non-echo requests are
		// dropped: control conns carry only heartbeats.
		reply := synthesizeEchoReply(ip)
		config.LPool.Put(buf[:])
		if reply == nil {
			continue
		}
		if err := writeDL.refresh(); err != nil {
			return
		}
		frame := make([]byte, typePrefixLen+len(reply))
		frame[0] = packetTypeToTUN
		copy(frame[typePrefixLen:], reply)
		token := write.begin()
		_, werr := tcpConn.Write(frame)
		write.finish(token, 1, len(frame), werr == nil && ctx.Err() == nil)
		if werr != nil {
			plog.G(ctx).Errorf("[Gvisor-TCP] Failed to write heartbeat reply on control conn: %v", werr)
			return
		}
	}
}

// synthesizeEchoReply builds the echo reply for an IPv4/ICMPv6 echo request by swapping the
// addresses, flipping the ICMP type and recomputing checksums. The ICMP body is copied
// verbatim so the client's probe correlation (nonce, generation, counter, ident/seq)
// survives. Returns nil when ip is not an echo request.
func synthesizeEchoReply(ip []byte) []byte {
	switch {
	case netutil.IsIPv4(ip):
		ihl := int(ip[0]&0x0f) * 4
		if ihl < 20 || len(ip) < ihl+8 || ip[9] != byte(header.ICMPv4ProtocolNumber) || header.ICMPv4Type(ip[ihl]) != header.ICMPv4Echo {
			return nil
		}
		out := bytes.Clone(ip)
		copy(out[12:16], ip[16:20])
		copy(out[16:20], ip[12:16])
		out[ihl] = byte(header.ICMPv4EchoReply)
		icmp := header.ICMPv4(out[ihl:])
		icmp.SetChecksum(0)
		icmp.SetChecksum(header.ICMPv4Checksum(icmp, 0))
		v4 := header.IPv4(out)
		v4.SetChecksum(0)
		v4.SetChecksum(^v4.CalculateChecksum())
		return out
	case netutil.IsIPv6(ip):
		const ipv6HeaderLen = 40
		if len(ip) < ipv6HeaderLen+8 || ip[6] != byte(header.ICMPv6ProtocolNumber) || header.ICMPv6Type(ip[ipv6HeaderLen]) != header.ICMPv6EchoRequest {
			return nil
		}
		out := bytes.Clone(ip)
		copy(out[8:24], ip[24:40])
		copy(out[24:40], ip[8:24])
		out[ipv6HeaderLen] = byte(header.ICMPv6EchoReply)
		v6 := header.IPv6(out)
		icmp := header.ICMPv6(out[ipv6HeaderLen:])
		icmp.SetChecksum(0)
		icmp.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
			Header: icmp, Src: v6.SourceAddress(), Dst: v6.DestinationAddress(),
		}))
		return out
	default:
		return nil
	}
}
