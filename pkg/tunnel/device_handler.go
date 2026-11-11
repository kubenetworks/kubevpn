package tunnel

import (
	"context"
	"net"
	"sync"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

// transport is the role-specific half of the symmetric tunDevice: how outbound packets (read
// from the TUN) are routed toward the wire, and the goroutines that feed inbound packets back
// into the device's tunOutbound. clientTransport dials a connection pool to the server;
// serverTransport routes via Router and bridges the gvisor TUN queue.
type transport interface {
	// label prefixes the device's read-tun logs, e.g. "[Client]" or "[TUN]".
	label() string
	// routeOutbound consumes one owning packet read from the TUN.
	routeOutbound(ctx context.Context, packet *packet)
	// routines returns the transport's role-specific goroutines.
	routines() []namedRoutine
}

// namedRoutine is a labeled goroutine, for uniform lifecycle tracking and start/stop logging.
type namedRoutine struct {
	name string
	fn   func(ctx context.Context)
}

type tunHandler struct {
	forward *DialConfig
	hub     *Router
	errChan chan error
	// stats records data-plane liveness from observed heartbeat echo replies (client role only);
	// may be nil.
	stats *HeartbeatStats
}

// TunHandler creates a handler for the tun tunnel. The same handler serves both roles, decided
// per connection: a configured forwarder ⇒ client (local machine / sidecar), none ⇒ server
// (traffic manager hub). stats is observed by the client role to track heartbeat reply liveness
// and may be nil (server role, or when liveness tracking is not needed).
func TunHandler(forward *DialConfig, hub *Router, stats *HeartbeatStats) Handler {
	return &tunHandler{
		forward: forward,
		hub:     hub,
		errChan: make(chan error, 1),
		stats:   stats,
	}
}

// FatalOnReturn marks only the shared server device; client completion belongs
// to NetworkManager through its existing completion handler.
func (h *tunHandler) FatalOnReturn() bool { return h.forward.IsEmpty() }

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	dev, err := newTunDevice(tun, h.errChan)
	if err != nil {
		plog.G(ctx).Errorf("[TUN] Failed to get tun device: %v", err)
		return
	}
	ctx = plog.WithField(ctx, plog.FieldTun, dev.tunName)

	if h.forward.IsEmpty() {
		dev.transport = newServerTransport(dev, h.hub)
	} else {
		dev.transport = newClientTransport(dev, h.forward, h.stats)
	}
	serve(ctx, dev)
}

// deviceSession owns the device context, diagnostics and every routine. Its
// shutdown order is cancel -> close device -> join routines -> transport/device drain.
type deviceSession struct {
	dev         *tunDevice
	diagnostics *datapathDiagnostics
	workers     sync.WaitGroup
}

func serve(ctx context.Context, dev *tunDevice) {
	session := &deviceSession{dev: dev, diagnostics: newDatapathDiagnostics()}
	session.diagnostics.drops = &dev.drops
	session.run(ctx)
}

func (s *deviceSession) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = withDatapathDiagnostics(ctx, s.diagnostics)
	s.start(ctx)

	select {
	case err := <-s.dev.errChan:
		if err != nil {
			plog.G(ctx).Errorf("[TUN] device exited: %v", err)
		}
	case <-ctx.Done():
	}
	cancel()
	s.closeAndJoin()
}

func (s *deviceSession) start(ctx context.Context) {
	s.workers.Add(1)
	go func() { defer s.workers.Done(); s.diagnostics.run(ctx) }()
	for _, r := range s.dev.routines() {
		s.workers.Add(1)
		go func(r namedRoutine) {
			defer s.workers.Done()
			defer netutil.HandleCrash()
			plog.G(ctx).Debugf("[TUN] routine %q started", r.name)
			r.fn(ctx)
			plog.G(ctx).Debugf("[TUN] routine %q stopped", r.name)
		}(r)
	}
}

func (s *deviceSession) closeAndJoin() {
	s.dev.Close()
	s.workers.Wait()
	// A canceled select can still enqueue into a ready channel after its
	// consumer's deferred drain. Only now have all device producers joined.
	if closer, ok := s.dev.transport.(interface{ close() }); ok {
		closer.close()
	}
	drainPacketChan(s.dev.tunInbound)
	drainPacketChan(s.dev.tunOutbound)
}
