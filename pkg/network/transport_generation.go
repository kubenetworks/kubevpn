package network

import (
	"context"
	"net"
	"sync"

	"github.com/wencaiwulue/kubevpn/v2/pkg/endpoint"
)

// transportGeneration owns all resources created for one outer transport.
// Lock order: applyMu -> supervisor.mu -> generation.mu -> HeartbeatStats.
// No network I/O or blocking Close/Stop runs while a metadata mutex is held.
type transportGeneration struct {
	id         uint64
	kind       transportKind
	stage      string
	diagnostic *transportDiagnostics
	ctx        context.Context
	cancel     context.CancelFunc
	endpoints  transportEndpoints
	handles    []transportHandle

	mu         sync.Mutex
	retired    bool
	conns      map[*generationConn]struct{}
	dials      sync.WaitGroup
	applyMu    sync.Mutex
	retireOnce sync.Once
}

type transportEvent struct {
	generation uint64
	err        error
}

type generationConn struct {
	net.Conn
	generation *transportGeneration
	once       sync.Once
	err        error
}

func (c *generationConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.generation.mu.Lock()
		delete(c.generation.conns, c)
		c.generation.mu.Unlock()
	})
	return c.err
}

type generationTransporter struct {
	nm       *transportSupervisor
	base     endpoint.Transporter
	endpoint transportEndpoint
}

func (t *generationTransporter) Dial(ctx context.Context, _ string) (net.Conn, error) {
	return t.nm.dialTransport(ctx, t.endpoint, t.base)
}

func newTransportGeneration(ctx context.Context, kind transportKind) *transportGeneration {
	gctx, cancel := context.WithCancel(ctx)
	return &transportGeneration{
		kind: kind, stage: "configuration", ctx: gctx, cancel: cancel,
		conns: make(map[*generationConn]struct{}),
	}
}

// beginDial admits work only while g is the active, non-retired generation.
// Add is serialized with retirement's retired check before dials.Wait.
func (g *transportGeneration) beginDial(nm *transportSupervisor) bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired || g.ctx.Err() != nil || nm.active != g {
		return false
	}
	g.dials.Add(1)
	return true
}

// publishDial performs the second stale fence after Dial/TLS handshake. A
// connection is registered only when the whole operation is still valid.
func (g *transportGeneration) publishDial(nm *transportSupervisor, conn net.Conn, valid bool) (*generationConn, bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	stale := g.retired || nm.active != g
	if valid && !stale {
		c := &generationConn{Conn: conn, generation: g}
		g.conns[c] = struct{}{}
		return c, false
	}
	return nil, stale
}

// retireResources cancels generation work before taking apply ownership, then
// closes every published connection and endpoint handle outside metadata locks.
func (g *transportGeneration) retireResources(nm *transportSupervisor) {
	g.retireOnce.Do(func() {
		g.cancel()
		g.applyMu.Lock()
		nm.mu.Lock()
		g.mu.Lock()
		g.retired = true
		if nm.active == g {
			nm.active = nil
			nm.heartbeatStats.BeginGeneration()
			nm.notifyTransportLocked()
		}
		conns := make([]*generationConn, 0, len(g.conns))
		for c := range g.conns {
			conns = append(conns, c)
		}
		g.mu.Unlock()
		nm.mu.Unlock()
		g.applyMu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		for _, h := range g.handles {
			h.Stop()
		}
		g.dials.Wait()
	})
}
