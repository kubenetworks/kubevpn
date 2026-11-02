package tunnel

import (
	"context"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

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

// peerRegistry owns per-IP stacks and grace timers under hub.mu -> mu.
type peerRegistry struct {
	hub            *Router
	newStack       stackConstructor
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	clients        map[string]*clientStack
	afterFunc      func(time.Duration, func()) graceTimer
	workers        sync.WaitGroup
	stopped        bool
	diagnostics    *datapathDiagnostics
	diagnosticDone chan struct{}
	nextConnID     uint64 // protected by mu; server-local identity
	drops          datapathDrops
}

// Start binds peers to the service, never to its first accepted connection.
func (h *peerRegistry) Start(ctx context.Context) {
	h.hub.mu.Lock()
	h.mu.Lock()
	if h.ctx == nil && !h.stopped {
		h.diagnostics = newDatapathDiagnostics()
		h.diagnostics.drops = &h.drops
		h.ctx, h.cancel = context.WithCancel(withDatapathDiagnostics(ctx, h.diagnostics))
		h.diagnosticDone = make(chan struct{})
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			defer close(h.diagnosticDone)
			h.diagnostics.run(h.ctx)
		}()
	}
	h.mu.Unlock()
	h.hub.mu.Unlock()
}

// Close is terminal and joins construction, output and stack destruction.
func (h *peerRegistry) Close() error {
	h.hub.mu.Lock()
	h.mu.Lock()
	h.stopped = true
	cancel := h.cancel
	h.mu.Unlock()
	h.hub.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	h.workers.Wait()
	return nil
}

// getOrCreateClientStack returns the gvisor stack for the given client IP, creating one
// if it doesn't exist yet.
func (h *peerRegistry) getOrCreateClientStack(srcKey string) *clientStack {
	h.hub.mu.Lock()
	h.mu.Lock()
	parent := h.ctx
	cs := h.clients[srcKey]
	if h.stopped || parent == nil || parent.Err() != nil {
		h.mu.Unlock()
		h.hub.mu.Unlock()
		return nil
	}
	if cs == nil {
		h.workers.Add(1)
	}
	h.mu.Unlock()
	h.hub.mu.Unlock()
	if cs != nil {
		return cs
	}

	// Construction can be expensive and must not stall unrelated route operations.
	stackCtx, stackCancel := context.WithCancel(plog.WithField(parent, "peer_tun_ip", net.IP(srcKey).String()))
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
	publish := !h.stopped && parent.Err() == nil && existing == nil
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
		h.workers.Done()
		if parent.Err() != nil {
			return nil
		}
		return existing
	}

	native := diagnosticGvisor(stackCtx, s, endpoint, MaxSize, "server_client", net.IP(srcKey))
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		defer netutil.HandleCrash()
		h.readFromEndpointWriteToRoute(stackCtx, endpoint)
	}()
	go func() {
		defer h.workers.Done()
		<-stackCtx.Done()
		native.close()
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
func (h *peerRegistry) onRouteEmptyLocked(ipKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cs := h.clients[ipKey]; cs != nil {
		h.armGraceLocked(ipKey, cs)
	}
}

func (h *peerRegistry) onRouteAddedLocked(ipKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cs := h.clients[ipKey]; cs != nil {
		h.invalidateGraceLocked(cs)
	}
}

func (h *peerRegistry) invalidateGraceLocked(cs *clientStack) {
	cs.graceEpoch++
	cs.graceArmed = false
	if cs.graceTimer != nil {
		cs.graceTimer.Stop()
		cs.graceTimer = nil
	}
}

func (h *peerRegistry) armGraceLocked(ipKey string, cs *clientStack) {
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

func (h *peerRegistry) expireGrace(ipKey string, expected *clientStack, epoch uint64) {
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
