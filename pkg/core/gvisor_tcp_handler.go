package core

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// stackConstructor is the strategy for creating a gvisor network stack from a TUN
// link endpoint. Two implementations exist: NewStack (forwards to real destinations)
// and NewLocalStack (forwards to 127.0.0.1, for testing/dev).
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

// clientStack manages an independent gvisor stack for one client (identified by TUN IP).
// Its lifetime spans the client session, surviving individual pool-slot reconnections.
type clientStack struct {
	endpoint *channel.Endpoint
	stack    *stack.Stack
	cancel   context.CancelFunc

	done chan struct{} // output loop and stack destruction have both completed

	// All grace state is protected by handler.mu, within a hub transaction.
	graceTimer graceTimer
	graceEpoch uint64
	graceArmed bool
}

type graceTimer interface{ Stop() bool }

// gvisorTCPHandler accepts TCP tunnel connections and forwards their traffic through
// per-client gvisor stacks. Each client (unique TUN IP) gets an independent stack that
// outlives individual connection drops, preserving TCP sessions across reconnections while
// isolating clients from each other's backpressure.
type gvisorTCPHandler struct {
	hub      *RouteHub
	newStack stackConstructor
	ctx      context.Context

	mu        sync.Mutex
	clients   map[string]*clientStack
	afterFunc func(time.Duration, func()) graceTimer // scheduler must not call inline
}

// GvisorTCPHandler creates a Handler that accepts TCP connections and maintains per-client
// gvisor stacks. Each client's stack survives individual connection disconnects, preserving
// TCP sessions across reconnections.
func GvisorTCPHandler(hub *RouteHub) Handler {
	h := &gvisorTCPHandler{
		hub:      hub,
		newStack: NewStack,
		clients:  make(map[string]*clientStack),
	}
	if hub != nil {
		hub.mu.Lock()
		hub.emptyLocked = h.onRouteEmptyLocked
		hub.addedLocked = h.onRouteAddedLocked
		hub.mu.Unlock()
	}
	return h
}

// GvisorLocalTCPHandler creates a Handler using NewLocalStack (routes to 127.0.0.1).
func GvisorLocalTCPHandler(hub *RouteHub) Handler {
	h := &gvisorTCPHandler{
		hub:      hub,
		newStack: NewLocalStack,
		clients:  make(map[string]*clientStack),
	}
	if hub != nil {
		hub.mu.Lock()
		hub.emptyLocked = h.onRouteEmptyLocked
		hub.addedLocked = h.onRouteAddedLocked
		hub.mu.Unlock()
	}
	return h
}

func (h *gvisorTCPHandler) Handle(ctx context.Context, tcpConn net.Conn) {
	defer tcpConn.Close()
	h.mu.Lock()
	if h.ctx == nil {
		h.ctx = ctx
	}
	h.mu.Unlock()
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	plog.G(connCtx).Debugf("[Gvisor-TCP] Client connected: %s", tcpConn.RemoteAddr())
	h.readFromTCPConnWriteToEndpoint(connCtx, newBufferedTCP(connCtx, tcpConn, func(c *bufferedTCP) {
		h.hub.RemoveRoutesByConn(connCtx, c)
	}))
	plog.G(connCtx).Debugf("[Gvisor-TCP] Client disconnected: %s", tcpConn.RemoteAddr())
}

// getOrCreateClientStack returns the gvisor stack for the given client IP, creating one
// if it doesn't exist yet.
func (h *gvisorTCPHandler) getOrCreateClientStack(srcKey string) *clientStack {
	h.hub.mu.Lock()
	h.mu.Lock()
	parent := h.ctx
	cs := h.clients[srcKey]
	if parent == nil || parent.Err() != nil {
		h.mu.Unlock()
		h.hub.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	h.hub.mu.Unlock()
	if cs != nil {
		return cs
	}

	// Construction can be expensive and must not stall unrelated route operations.
	stackCtx, stackCancel := context.WithCancel(parent)
	endpoint := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	endpoint.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	endpoint.SupportedGSOKind = stack.GVisorGSOSupported

	s := h.newStack(stackCtx, sniffLink(stackCtx, endpoint, "[gVISOR] "))

	cs = &clientStack{
		endpoint: endpoint,
		stack:    s,
		cancel:   stackCancel,
		done:     make(chan struct{}),
	}
	h.hub.mu.Lock()
	h.mu.Lock()
	existing := h.clients[srcKey]
	publish := parent.Err() == nil && existing == nil
	if publish {
		h.clients[srcKey] = cs
		if h.hub.routes[srcKey] == nil {
			h.armGraceLocked(srcKey, cs)
		}
	}
	h.mu.Unlock()
	h.hub.mu.Unlock()
	if !publish {
		stackCancel()
		s.Destroy()
		close(cs.done)
		if parent.Err() != nil {
			return nil
		}
		return existing
	}

	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		defer netutil.HandleCrash()
		h.readFromEndpointWriteToRoute(stackCtx, endpoint)
	}()
	go func() {
		<-stackCtx.Done()
		h.hub.mu.Lock()
		h.mu.Lock()
		if h.clients[srcKey] == cs {
			delete(h.clients, srcKey)
		}
		h.invalidateGraceLocked(cs)
		h.mu.Unlock()
		h.hub.mu.Unlock()
		s.Destroy()
		<-outputDone
		close(cs.done)
	}()

	plog.G(parent).Debugf("[Gvisor-TCP] Created per-client stack for %s", net.IP(srcKey))
	return cs
}

// These callbacks run inside the hub transaction, preserving empty/add order.
func (h *gvisorTCPHandler) onRouteEmptyLocked(ipKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cs := h.clients[ipKey]; cs != nil {
		h.armGraceLocked(ipKey, cs)
	}
}

func (h *gvisorTCPHandler) onRouteAddedLocked(ipKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cs := h.clients[ipKey]; cs != nil {
		h.invalidateGraceLocked(cs)
	}
}

func (h *gvisorTCPHandler) invalidateGraceLocked(cs *clientStack) {
	cs.graceEpoch++
	cs.graceArmed = false
	if cs.graceTimer != nil {
		cs.graceTimer.Stop()
		cs.graceTimer = nil
	}
}

func (h *gvisorTCPHandler) armGraceLocked(ipKey string, cs *clientStack) {
	if cs.graceArmed {
		return
	}
	cs.graceEpoch++
	cs.graceArmed = true
	epoch := cs.graceEpoch
	after := h.afterFunc
	if after == nil {
		after = func(d time.Duration, f func()) graceTimer { return time.AfterFunc(d, f) }
	}
	cs.graceTimer = after(config.KeepAliveTime*3, func() {
		h.expireGrace(ipKey, cs, epoch)
	})
}

func (h *gvisorTCPHandler) expireGrace(ipKey string, expected *clientStack, epoch uint64) {
	h.hub.mu.Lock()
	h.mu.Lock()
	valid := h.clients[ipKey] == expected && expected.graceArmed &&
		expected.graceEpoch == epoch && h.hub.routes[ipKey] == nil
	if valid {
		delete(h.clients, ipKey)
		h.invalidateGraceLocked(expected)
	}
	h.mu.Unlock()
	h.hub.mu.Unlock()
	if valid {
		expected.cancel()
	}
}

// handleControlConn serves a dedicated control-plane connection. It does NOT register the conn
// in RouteHub (no AddRoute), so it never receives data-plane traffic. It reads heartbeat ICMP
// echoes from the client, injects them into the client's gvisor stack for processing, and writes
// the echo replies directly back on this conn.
func (h *gvisorTCPHandler) handleControlConn(ctx context.Context, tcpConn net.Conn) {
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

		// Inject heartbeat ICMP into the client's per-client gvisor stack.
		// The stack's ICMP forwarder generates a reply, which readFromEndpointWriteToRoute
		// routes back to the client via RouteHub (through data conns, not this control conn).
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
		cs.endpoint.InjectInbound(protocol, pkt)
		pkt.DecRef()
	}
}

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
