package core

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func lifetimeAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout: %s", what)
	}
}

// Real TCP with an explicit failure barrier, rather than timing-dependent
// socket-buffer saturation. Reads stay live while the writer is blocked.
type lifetimeFailConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *lifetimeFailConn) Write([]byte) (int, error) {
	close(c.entered)
	<-c.release
	return 0, net.ErrClosed
}

// The second snapshot member must be revalidated after the first write returns.
func TestRouteLifetime_RemovedFallbackIsNotAttempted(t *testing.T) {
	for _, mode := range []string{"removed", "readded", "replacement-list"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			hub := NewRouteHub()
			ip := net.IPv4(198, 18, 0, 61).To4()
			_, first := batchTCPPair(t, false)
			_, second := batchTCPPair(t, false)
			hub.AddRoute(ctx, ip, second)
			hub.AddRoute(ctx, ip, first)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var attempted atomic.Bool
			var writeErr error
			go func() {
				_, writeErr = hub.WriteFuncToRoute(string(ip), func(c net.Conn) error {
					if c == first {
						close(entered)
						<-release
						return net.ErrClosed
					}
					attempted.Store(true)
					return nil
				})
				close(done)
			}()
			lifetimeAwait(t, entered, "first write")
			hub.RemoveRoutesByConn(ctx, second)
			if mode == "replacement-list" {
				hub.RemoveRoutesByConn(ctx, first)
			}
			if mode != "removed" {
				hub.AddRoute(ctx, ip, second)
			}
			once.Do(func() { close(release) })
			lifetimeAwait(t, done, "fallback completion")
			if attempted.Load() || writeErr == nil {
				t.Fatal("removed snapshot member was attempted")
			}
			if mode != "removed" && !hub.HasRoute(string(ip)) {
				t.Fatal("new registration was pruned")
			}
		})
	}
}

func TestStage3_LegacyWriteFuncKeepsFallback(t *testing.T) {
	_, first := batchTCPPair(t, false)
	_, fallback := batchTCPPair(t, false)
	hub := NewRouteHub()
	ctx := context.Background()
	ip := net.IPv4(198, 18, 0, 77).To4()
	hub.AddRoute(ctx, ip, fallback)
	hub.AddRoute(ctx, ip, first)
	used, err := hub.WriteFuncToRoute(string(ip), func(c net.Conn) error {
		if c == first {
			return context.Canceled
		}
		_, err := c.Write([]byte{1})
		return err
	})
	if err != nil || used != fallback {
		t.Fatalf("legacy callback lost fallback semantics: %v", err)
	}
	members := routeSnapshot(hub)[string(ip)]
	if len(members) != 1 || members[0] != fallback {
		t.Fatal("failed callback member was not removed")
	}
}

func TestRouteLifetime_RawGateRevalidatesSnapshot(t *testing.T) {
	_, raw := batchTCPPair(t, false)
	cl := &ConnList{}
	cl.Add(raw)
	members := cl.snapshot()
	if err := cl.lockWrite(context.Background()); err != nil {
		t.Fatal(err)
	}
	entered, done := make(chan struct{}), make(chan struct{})
	var attempted atomic.Bool
	go func() {
		close(entered)
		_, _ = cl.writeMembers(context.Background(), members, func(net.Conn) (int, error) { attempted.Store(true); return 0, nil }, routeWriteCallback, cl.containsMember, cl.prepareWriteFailure)
		close(done)
	}()
	lifetimeAwait(t, entered, "snapshot writer started")
	// Re-add the identical conn under a new membership while the raw gate is held.
	cl.Remove(raw)
	cl.Add(raw)
	cl.unlockWrite()
	lifetimeAwait(t, done, "raw gate waiter")
	if attempted.Load() {
		t.Fatal("raw gate waiter attempted a removed snapshot member")
	}
	if cl.Len() != 1 {
		t.Fatal("raw gate waiter removed re-added membership")
	}
}

func lifetimeWrite(hub *RouteHub, api, key string) (net.Conn, error) {
	frame := []byte{0, 1, 0}
	switch api {
	case "bytes":
		return hub.WriteToRoute(key, frame)
	case "func":
		return hub.WriteFuncToRoute(key, func(c net.Conn) error { _, err := c.Write(frame); return err })
	default:
		buf := config.LPool.Get().([]byte)
		copy(buf, frame)
		pkt := NewPacket(buf, 1, nil, nil)
		defer pkt.release()
		return hub.WriteToRoutePacket(context.Background(), key, pkt)
	}
}

func TestRouteLifetime_WriteEvictionTransitions(t *testing.T) {
	for _, api := range []string{"bytes", "func", "packet"} {
		for _, fallback := range []bool{false, true} {
			name := api + "/empty"
			if fallback {
				name = api + "/fallback"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				hub := NewRouteHub()
				empty := 0
				hub.emptyLocked = func(string) { empty++ }
				ip := net.IPv4(198, 18, 0, 51).To4()
				var alive net.Conn
				if fallback {
					peer, raw := batchTCPPair(t, false)
					alive = NewBufferedTCP(ctx, raw)
					defer alive.Close()
					hub.AddRoute(ctx, ip, alive)
					defer peer.Close()
				}
				peer, raw := batchTCPPair(t, false)
				// No stop callback here: exercise write-side reconciliation itself.
				dead := NewBufferedTCP(ctx, raw)
				hub.AddRoute(ctx, ip, dead)
				_ = dead.Close()
				_ = peer.Close()
				used, err := lifetimeWrite(hub, api, string(ip))
				if fallback {
					if err != nil || used != alive {
						t.Fatalf("fallback: conn=%v err=%v", used, err)
					}
					members := routeSnapshot(hub)[string(ip)]
					if len(members) != 1 || members[0] != alive {
						t.Fatal("fallback success did not prune failed member")
					}
					if empty != 0 {
						t.Fatal("fallback emitted premature empty transition")
					}
					hub.RemoveRoutesByConn(ctx, alive)
				} else if err == nil {
					t.Fatal("closed writer succeeded")
				}
				if empty != 1 {
					t.Fatalf("write eviction emitted %d empty transitions, want 1", empty)
				}
				hub.RemoveRoutesByConn(ctx, dead)
				if empty != 1 {
					t.Fatal("deferred removal repeated empty transition")
				}
			})
		}
	}
}

func TestRouteLifetime_ClosedCannotRegister(t *testing.T) {
	peer, raw := batchTCPPair(t, false)
	defer peer.Close()
	hub := NewRouteHub()
	ctx := context.Background()
	stopped := make(chan struct{})
	conn := newBufferedTCP(ctx, raw, func(c *bufferedTCP) {
		hub.RemoveRoutesByConn(ctx, c)
		close(stopped)
	})
	ip := net.IPv4(198, 18, 0, 52).To4()
	hub.AddRoute(ctx, ip, conn)
	_ = conn.Close()
	lifetimeAwait(t, stopped, "stop hook")
	if hub.AddRoute(ctx, ip, conn) || hub.HasRoute(string(ip)) {
		t.Fatal("stopped buffered writer registered a stale route")
	}
}

// Blocks either after a complete first payload has been read (before AddRoute),
// or at the next header read (after that payload has registered and been injected).
type lifetimeReadBarrier struct {
	net.Conn
	beforeAdd bool
	reads     int
	entered   chan struct{}
	release   chan struct{}
}

func (c *lifetimeReadBarrier) Read(b []byte) (int, error) {
	c.reads++
	if !c.beforeAdd && c.reads == 3 {
		close(c.entered)
		<-c.release
	}
	n, err := io.ReadFull(c.Conn, b)
	if c.beforeAdd && c.reads == 2 {
		close(c.entered)
		<-c.release
	}
	return n, err
}

func TestRouteLifetime_FramedReaderStopRegistrationRace(t *testing.T) {
	for _, beforeAdd := range []bool{true, false} {
		name := "add-before-stop"
		if beforeAdd {
			name = "stop-before-add"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewRouteHub()
			h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
			h.ctx = ctx
			scheduler := &lifetimeScheduler{timers: make(chan *lifetimeTimer, 8)}
			h.afterFunc = scheduler.after
			peer, raw := batchTCPPair(t, false)
			barrier := &lifetimeReadBarrier{Conn: raw, beforeAdd: beforeAdd, entered: make(chan struct{}), release: make(chan struct{})}
			conn := newBufferedTCP(ctx, barrier, func(c *bufferedTCP) { hub.RemoveRoutesByConn(ctx, c) })
			var release sync.Once
			defer release.Do(func() { close(barrier.release) })
			defer conn.Close()
			done := make(chan struct{})
			go func() { h.readFromTCPConnWriteToEndpoint(ctx, conn); close(done) }()
			ip := net.IPv4(198, 18, 0, 62).To4()
			sendFramedPacket(peer, packetTypeToGvisor, genEchoRequest(t, ip.String(), config.RouterIP.String()))
			lifetimeAwait(t, barrier.entered, "framed reader barrier")
			if !beforeAdd && !hub.HasRoute(string(ip)) {
				t.Fatal("first frame did not register")
			}
			_ = conn.Close()
			if hub.HasRoute(string(ip)) {
				t.Fatal("stop left a route registered")
			}
			release.Do(func() { close(barrier.release) })
			lifetimeAwait(t, done, "stopped framed reader")
			if hub.HasRoute(string(ip)) {
				t.Fatal("old reader resurrected its route")
			}
			h.mu.Lock()
			cs := h.clients[string(ip)]
			h.mu.Unlock()
			if beforeAdd {
				if cs != nil {
					t.Fatal("stopped old reader created a fresh stack")
				}
			} else {
				if cs == nil {
					t.Fatal("first frame never created stack")
				}
				scheduler.next(t).fn()
				lifetimeAwait(t, cs.done, "registered stack reclaimed after stop")
			}
		})
	}
}

func TestStackLifetime_EstablishedTCPContinuesAcrossReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
		defer c.Close()
		stopClose := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stopClose()
		_, _ = io.Copy(c, c)
	}()
	defer func() {
		cancel()
		_ = ln.Close()
		lifetimeAwait(t, echoDone, "echo service cleanup on every exit")
	}()

	hub := NewRouteHub()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	scheduler := &lifetimeScheduler{timers: make(chan *lifetimeTimer, 8)}
	h.afterFunc = scheduler.after
	ip := net.IPv4(198, 18, 0, 63).To4()
	ep := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	ep.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	origin := newGvisorStack(ctx, ep, LocalTCPForwarder, LocalUDPForwarder)
	addIPv4ToStack(t, origin, 1, ip)
	defer origin.Destroy()

	// Each transport gets new framing readers/writers, but both gvisor stacks
	// and the application TCP connection stay alive throughout the outage.
	connect := func() func() {
		peer, raw := batchTCPPair(t, false)
		transportCtx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); h.Handle(ctx, raw) }()
		go func() { defer wg.Done(); bridgePipeToEndpoint(transportCtx, peer, ep) }()
		// Announce on this transport before the endpoint writer can emit frames.
		sendFramedPacket(peer, packetTypeToGvisor, genEchoRequest(t, ip.String(), config.RouterIP.String()))
		go func() {
			defer wg.Done()
			for transportCtx.Err() == nil {
				pkt := ep.ReadContext(transportCtx)
				if pkt == nil {
					return
				}
				view := pkt.ToView()
				frame := frameDatagram(packetTypeToGvisor, view.AsSlice())
				view.Release()
				pkt.DecRef()
				if _, err := peer.Write(frame); err != nil {
					return
				}
			}
		}()
		return func() {
			stop()
			_ = peer.Close()
			_ = raw.Close()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			lifetimeAwait(t, done, "transport bridge cleanup")
		}
	}
	stopTransport := connect()
	defer func() { stopTransport() }()
	remote := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 96, 0, 10}), Port: uint16(ln.Addr().(*net.TCPAddr).Port)}
	app, err := gonet.DialContextTCP(ctx, origin, remote, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var service net.Conn
	select {
	case service = <-accepted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = service.Close(); lifetimeAwait(t, echoDone, "business service cleanup") }()
	exchange := func(text string) {
		t.Helper()
		_ = app.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := app.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(text))
		if _, err := io.ReadFull(app, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != text {
			t.Fatalf("business echo = %q, want %q", got, text)
		}
	}
	exchange("established before reconnect")
	h.mu.Lock()
	retained := h.clients[string(ip)]
	h.mu.Unlock()
	defer func() { cancel(); lifetimeAwait(t, retained.done, "retained server stack cleanup") }()
	stopTransport()
	stopTransport = func() {}
	stale := scheduler.next(t)
	if hub.HasRoute(string(ip)) {
		t.Fatal("old data transport still registered")
	}
	// Observe registration of the replacement transport before firing old expiry.
	registered := make(chan struct{}, 1)
	hub.mu.Lock()
	added := hub.addedLocked
	hub.addedLocked = func(key string) {
		added(key)
		if key == string(ip) {
			registered <- struct{}{}
		}
	}
	hub.mu.Unlock()
	stopTransport = connect()
	lifetimeAwait(t, registered, "replacement data registration")
	stale.fn()
	exchange("same application connection after stale expiry")
	h.mu.Lock()
	current := h.clients[string(ip)]
	h.mu.Unlock()
	if current != retained {
		t.Fatal("server replaced established TCP stack")
	}
}

func lifetimeHeartbeat(t *testing.T, cs *clientStack, peer net.Conn, ip net.IP) {
	t.Helper()
	pkt := buildICMPv4EchoRequest(tcpip.AddrFrom4([4]byte(ip.To4())), tcpip.AddrFrom4([4]byte(config.RouterIP.To4())), 17, 1, []byte("idle"))
	cs.endpoint.InjectInbound(header.IPv4ProtocolNumber, pkt)
	pkt.DecRef()
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	prefix, reply, err := readDatagram(peer)
	if err != nil || prefix != packetTypeToTUN || !isEchoReply(reply) {
		t.Fatalf("idle heartbeat after reconnect: prefix=%d err=%v reply=%x", prefix, err, reply)
	}
}

func TestRouteLifetime_BlockedWriteAllowsReconnect(t *testing.T) {
	for _, mode := range []string{"same-list", "replacement-list", "readded-member"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewRouteHub()
			h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
			h.ctx = ctx
			ip := net.IPv4(198, 18, 0, 53).To4()
			peer, raw := batchTCPPair(t, false)
			defer peer.Close()
			dead := &lifetimeFailConn{Conn: raw, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(dead.release) })
			hub.AddRoute(ctx, ip, dead)
			cs := h.getOrCreateClientStack(string(ip))
			defer func() { cancel(); lifetimeAwait(t, cs.done, "stack cleanup") }()
			written := make(chan struct{})
			go func() { _, _ = hub.WriteToRoute(string(ip), []byte{0, 1, 0}); close(written) }()
			lifetimeAwait(t, dead.entered, "writer barrier")
			freshPeer, freshRaw := batchTCPPair(t, false)
			fresh := NewBufferedTCP(ctx, freshRaw)
			defer fresh.Close()
			added := make(chan struct{})
			go func() {
				if mode != "same-list" {
					// Keep a sibling membership when testing exact-member ABA.
					if mode == "readded-member" {
						hub.AddRoute(ctx, ip, fresh)
					}
					hub.RemoveRoutesByConn(ctx, dead)
					if mode == "readded-member" {
						hub.AddRoute(ctx, ip, dead)
					}
				}
				hub.AddRoute(ctx, ip, fresh)
				// Another client's metadata and write must also make progress.
				sibling := net.IPv4(198, 18, 0, 99).To4()
				hub.AddRoute(ctx, sibling, fresh)
				_, _ = hub.WriteToRoute(string(sibling), []byte{0, 1, 0})
				hub.RemoveRoutesByConn(ctx, nil)
				close(added)
			}()
			lifetimeAwait(t, added, "registration/sibling operation blocked by network write")
			release.Do(func() { close(dead.release) })
			lifetimeAwait(t, written, "old failed write reconciliation")
			members := routeSnapshot(hub)[string(ip)]
			want := 1
			if mode == "readded-member" {
				want = 2
			}
			if len(members) != want {
				t.Fatalf("lost reconnected membership: got %d want %d", len(members), want)
			}
			if mode == "readded-member" {
				// The old write must not prune the new registration of the same conn.
				hub.RemoveRoutesByConn(ctx, dead)
			}
			_ = freshPeer.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, _, err := readDatagram(freshPeer); err != nil {
				t.Fatal(err)
			} // sibling frame
			lifetimeHeartbeat(t, cs, freshPeer, ip) // no inbound data re-registration
		})
	}
}

// Stop intentionally does not suppress fire: models a callback already runnable
// when Stop loses the race. Tests fire even stopped and already-fired callbacks.
type lifetimeTimer struct {
	fn      func()
	stopped atomic.Bool
}

func (tm *lifetimeTimer) Stop() bool { return !tm.stopped.Swap(true) }

type lifetimeScheduler struct{ timers chan *lifetimeTimer }

func (s *lifetimeScheduler) after(d time.Duration, fn func()) graceTimer {
	if d != config.KeepAliveTime*3 {
		panic("unexpected grace duration")
	}
	tm := &lifetimeTimer{fn: fn}
	s.timers <- tm
	return tm
}
func (s *lifetimeScheduler) next(t *testing.T) *lifetimeTimer {
	t.Helper()
	select {
	case tm := <-s.timers:
		return tm
	case <-time.After(3 * time.Second):
		t.Fatal("grace was not armed")
		return nil
	}
}

func TestStackLifetime_ControlOnlyArmsGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouteHub()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	scheduler := &lifetimeScheduler{timers: make(chan *lifetimeTimer, 8)}
	h.afterFunc = scheduler.after
	peer, raw := batchTCPPair(t, false)
	handled := make(chan struct{})
	go func() { h.Handle(ctx, raw); close(handled) }()
	defer func() { _ = peer.Close(); lifetimeAwait(t, handled, "control reader cleanup") }()
	ip := net.IPv4(198, 18, 0, 54).To4()
	// The declaration is consumed separately; the following actual heartbeat
	// must create a stack even though there has never been a data route.
	sendFramedPacket(peer, packetTypeControl, nil)
	sendFramedPacket(peer, packetTypeControl, genEchoRequest(t, ip.String(), config.RouterIP.String()))
	orphan := scheduler.next(t)
	h.mu.Lock()
	cs := h.clients[string(ip)]
	h.mu.Unlock()
	if cs == nil || hub.HasRoute(string(ip)) {
		t.Fatal("control-before-data did not create an unrouted stack")
	}
	if h.getOrCreateClientStack(string(ip)) != cs {
		t.Fatal("control packet replaced stack")
	}
	select {
	case <-scheduler.timers:
		t.Fatal("control-only lookup refreshed orphan grace")
	default:
	}
	orphan.fn()
	lifetimeAwait(t, cs.done, "orphan stack output/destroy cleanup")
	h.mu.Lock()
	remaining := len(h.clients)
	h.mu.Unlock()
	if remaining != 0 {
		t.Fatal("orphan remains published")
	}
}

func TestStackLifetime_StaleTimersCannotDestroyRestoredOrReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouteHub()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	h.ctx = ctx
	scheduler := &lifetimeScheduler{timers: make(chan *lifetimeTimer, 8)}
	h.afterFunc = scheduler.after
	ip := net.IPv4(198, 18, 0, 55).To4()
	key := string(ip)
	cs := h.getOrCreateClientStack(key)
	old := scheduler.next(t)
	peer, raw := batchTCPPair(t, false)
	conn := NewBufferedTCP(ctx, raw)
	defer conn.Close()
	hub.AddRoute(ctx, ip, conn)
	if !old.stopped.Load() {
		t.Fatal("registration did not stop grace")
	}
	old.fn()
	if h.getOrCreateClientStack(key) != cs {
		t.Fatal("old timer destroyed restored stack")
	}
	lifetimeHeartbeat(t, cs, peer, ip)
	hub.RemoveRoutesByConn(ctx, conn)
	current := scheduler.next(t)
	old.fn() // no route again, but the epoch must still reject it
	if h.getOrCreateClientStack(key) != cs {
		t.Fatal("old epoch destroyed current grace stack")
	}
	current.fn()
	lifetimeAwait(t, cs.done, "expired stack cleanup")
	replacement := h.getOrCreateClientStack(key)
	replacementTimer := scheduler.next(t)
	current.fn() // same IP, different stack identity
	old.fn()
	if h.getOrCreateClientStack(key) != replacement {
		t.Fatal("stale timer destroyed replacement stack")
	}
	replacementTimer.fn()
	lifetimeAwait(t, replacement.done, "replacement cleanup")
}

func TestStackLifetime_WriterEvictionReclaimsStack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewRouteHub()
	h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
	scheduler := &lifetimeScheduler{timers: make(chan *lifetimeTimer, 8)}
	h.afterFunc = scheduler.after
	peer, raw := batchTCPPair(t, false)
	dead := &lifetimeFailConn{Conn: raw, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(dead.release) })
	handled := make(chan struct{})
	go func() { h.Handle(ctx, dead); close(handled) }()
	ip := net.IPv4(198, 18, 0, 56).To4()
	sendFramedPacket(peer, packetTypeToGvisor, genEchoRequest(t, ip.String(), config.RouterIP.String()))
	lifetimeAwait(t, dead.entered, "gvisor reply reached buffered writer")
	h.mu.Lock()
	cs := h.clients[string(ip)]
	h.mu.Unlock()
	if cs == nil || !hub.HasRoute(string(ip)) {
		t.Fatal("data registration/stack missing")
	}
	release.Do(func() { close(dead.release) })
	tm := scheduler.next(t)
	if hub.HasRoute(string(ip)) {
		t.Fatal("writer stop did not remove route")
	}
	tm.fn()
	lifetimeAwait(t, cs.done, "writer-evicted stack cleanup")
	lifetimeAwait(t, handled, "failed writer closed old reader")
	select {
	case <-scheduler.timers:
		t.Fatal("deferred removal repeated grace transition")
	default:
	}
}

func TestStackLifetime_ConstructionDoesNotHoldMetadata(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "duplicate"
		if canceled {
			name = "canceled-parent"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewRouteHub()
			h := GvisorLocalTCPHandler(hub).(*gvisorTCPHandler)
			h.ctx = ctx
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			contexts := make(chan context.Context, 2)
			h.newStack = func(ctx context.Context, ep stack.LinkEndpoint) *stack.Stack {
				contexts <- ctx
				entered <- struct{}{}
				<-release
				return NewLocalStack(ctx, ep)
			}
			ip := net.IPv4(198, 18, 0, 57).To4()
			result := make(chan *clientStack, 2)
			go func() { result <- h.getOrCreateClientStack(string(ip)) }()
			lifetimeAwait(t, entered, "first constructor")
			go func() { result <- h.getOrCreateClientStack(string(ip)) }()
			lifetimeAwait(t, entered, "concurrent constructor blocked by metadata")
			peer, raw := batchTCPPair(t, false)
			defer peer.Close()
			registered := make(chan struct{})
			go func() { hub.AddRoute(ctx, ip, raw); close(registered) }()
			lifetimeAwait(t, registered, "route registration during construction")
			if canceled {
				cancel()
			}
			once.Do(func() { close(release) })
			get := func() *clientStack {
				select {
				case cs := <-result:
					return cs
				case <-time.After(3 * time.Second):
					t.Fatal("construction did not return")
					return nil
				}
			}
			a, b := get(), get()
			if canceled {
				if a != nil || b != nil {
					t.Fatal("published a stack after parent cancellation")
				}
			} else {
				if a == nil || a != b {
					t.Fatal("duplicate construction did not converge")
				}
				c1, c2 := <-contexts, <-contexts
				if (c1.Err() == nil) == (c2.Err() == nil) {
					t.Fatal("duplicate candidate was not canceled")
				}
				cancel()
				lifetimeAwait(t, a.done, "published stack cleanup")
			}
			h.mu.Lock()
			remaining := len(h.clients)
			h.mu.Unlock()
			if remaining != 0 {
				t.Fatal("canceled stack remains published")
			}
		})
	}
}
