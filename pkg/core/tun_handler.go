package core

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
// serverTransport routes via RouteHub and bridges the gvisor TCPPacketChan.
type transport interface {
	// label prefixes the device's read-tun logs, e.g. "[Client]" or "[TUN]".
	label() string
	// routeOutbound handles one IP packet read from the TUN (framing headroom at buf[0:3],
	// IP data at buf[3:3+n]). The transport takes ownership of buf.
	routeOutbound(ctx context.Context, buf []byte, n int, src, dst net.IP)
	// routines returns the transport's role-specific goroutines.
	routines() []namedRoutine
}

// namedRoutine is a labeled goroutine, for uniform lifecycle tracking and start/stop logging.
type namedRoutine struct {
	name string
	fn   func(ctx context.Context)
}

type tunHandler struct {
	forward *Forwarder
	hub     *RouteHub
	errChan chan error
	// stats records data-plane liveness from observed heartbeat echo replies (client role only);
	// may be nil.
	stats *HeartbeatStats
}

// TunHandler creates a handler for the tun tunnel. The same handler serves both roles, decided
// per connection: a configured forwarder ⇒ client (local machine / sidecar), none ⇒ server
// (traffic manager hub). stats is observed by the client role to track heartbeat reply liveness
// and may be nil (server role, or when liveness tracking is not needed).
func TunHandler(forward *Forwarder, hub *RouteHub, stats *HeartbeatStats) Handler {
	return &tunHandler{
		forward: forward,
		hub:     hub,
		errChan: make(chan error, 1),
		stats:   stats,
	}
}

func (h *tunHandler) Handle(ctx context.Context, tun net.Conn) {
	tunIfi, err := netutil.GetTunDeviceByConn(tun)
	if err != nil {
		plog.G(ctx).Errorf("[TUN] Failed to get tun device: %v", err)
		return
	}
	ctx = plog.WithField(ctx, tunIfi.Name, "")

	dev := &tunDevice{
		tun:         tun,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     h.errChan,
	}
	if h.forward.IsEmpty() {
		dev.transport = newServerTransport(dev, h.hub)
	} else {
		dev.transport = newClientTransport(dev, h.forward, h.stats)
	}
	serve(ctx, dev)
}

// serve starts every device routine under a WaitGroup, waits for the first fatal error or
// context cancellation, then cancels, closes the TUN, and drains all routines. This gives a
// deterministic shutdown (WireGuard's peer.stopping pattern): each routine returns on ctx
// cancel, a closed TUN (unblocks pumpTun's read), or closed conns, so wg.Wait always completes.
func serve(ctx context.Context, dev *tunDevice) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for _, r := range dev.routines() {
		wg.Add(1)
		go func(r namedRoutine) {
			defer wg.Done()
			defer netutil.HandleCrash()
			plog.G(ctx).Debugf("[TUN] routine %q started", r.name)
			r.fn(ctx)
			plog.G(ctx).Debugf("[TUN] routine %q stopped", r.name)
		}(r)
	}

	select {
	case err := <-dev.errChan:
		if err != nil {
			plog.G(ctx).Errorf("[TUN] device exited: %v", err)
		}
	case <-ctx.Done():
	}
	cancel()
	dev.Close()
	wg.Wait()
}
