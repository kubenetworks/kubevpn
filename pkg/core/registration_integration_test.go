package core

// End-to-end test for proactive route registration — the mechanism that lets the server route a
// heartbeat echo reply back to an IDLE client.
//
// Why this exists: in production a client's data slots silently stopped announcing their TUN IP on
// (re)connect, because registrationPayloads() re-derived the address by scanning the whole OS
// interface table on every reconnect and that scan started failing. With no route, the server
// generated every echo reply and then dropped it ("No route for stack output -> x, dropping"), so
// the client's liveness watchdog never primed and force-reconnected the port-forward every 30s for
// 20 hours — which in turn starved the xDS lease renewal. One silent `return nil`.
//
// The wiring is the real data plane:
//
//	client tunDevice (net.Pipe as TUN) + clientTransport (1 data slot + 1 control slot)
//	  → loopback TCP → server gvisorTCPHandler + RouteHub
//	    → per-client gvisor stack answers the gateway echo
//	      → RouteHub routes the reply back over a DATA conn → client marks HeartbeatStats
//
// Note the existing inter-client ICMP e2e test has to call registerRoute() by hand — it was
// working around exactly this bug.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	logrus "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	netutil "github.com/wencaiwulue/kubevpn/v2/pkg/util/netutil"
)

const (
	regTestTunIPv4 = "198.18.0.5"
	regTestTunIPv6 = "2001:2::5"
)

// regServer is a server whose accepted connections can be force-closed, so a test can simulate the
// port-forward teardown that makes every client slot reconnect.
type regServer struct {
	hub  *RouteHub
	port int

	mu    sync.Mutex
	conns []net.Conn
}

func newRegServer(ctx context.Context, t testing.TB) *regServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &regServer{hub: NewRouteHub(), port: ln.Addr().(*net.TCPAddr).Port}
	handler := GvisorLocalTCPHandler(s.hub)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			go handler.Handle(ctx, conn)
		}
	}()
	return s
}

// dropAllConns force-closes every connection accepted so far, mimicking a port-forward teardown.
func (s *regServer) dropAllConns() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// routeConnCount returns how many conns the hub has registered for ip, or 0 when there is no route.
func (s *regServer) routeConnCount(ip net.IP) int {
	return len(routeSnapshot(s.hub)[regRouteKey(ip)])
}

// routeConns returns a snapshot of the exact conns the hub currently has registered for ip. Unlike
// routeConnCount, comparing conns by identity gives a stable observable across a teardown: the old
// conns are removed one by one while the client reconnects and registers fresh conns on the same
// key, so the aggregate count may never read 0, but "these specific conns are gone" always holds.
func (s *regServer) routeConns(ip net.IP) []net.Conn {
	return routeSnapshot(s.hub)[regRouteKey(ip)]
}

// routeKeyCount returns how many distinct addresses the hub currently has a route for.
func (s *regServer) routeKeyCount() int {
	return len(routeSnapshot(s.hub))
}

// regRouteKey mirrors how the server keys routes: the raw address bytes as ParseIPFast slices them
// out of the packet — 4 bytes for IPv4, 16 for IPv6.
func regRouteKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return string(v4)
	}
	return string(ip.To16())
}

// startRegClient builds a real clientTransport whose TUN is a net.Pipe and whose addresses come
// from the injected seam (no real TUN device, so no root and no interface-table dependency).
// addrs supplies the (v4, v6) the client believes it owns; both may be nil to model a client that
// cannot determine its own addresses. The OS side of the pipe is drained continuously because
// writeToTun blocks on an unbuffered net.Pipe.
func startRegClient(ctx context.Context, t testing.TB, serverPort int, v4, v6 net.IP) *HeartbeatStats {
	t.Helper()
	osSide, tunSide := net.Pipe()

	stats := &HeartbeatStats{}
	device := &tunDevice{
		tun:         tunSide,
		addrsFn:     func() (net.IP, net.IP, net.IP) { return v4, v6, nil },
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	device.transport = newClientTransport(device, &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}, stats)
	for _, r := range device.routines() {
		go r.fn(ctx)
	}
	t.Cleanup(func() { device.Close() })

	go func() {
		buf := make([]byte, config.LargeBufferSize)
		for ctx.Err() == nil {
			if _, err := osSide.Read(buf); err != nil {
				return
			}
		}
	}()
	return stats
}

// syncBuffer collects log output written concurrently by the data plane's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// unthrottleDataPlaneWarnings disables warning rate limiting so a test observes each diagnostic
// regardless of what earlier tests in this package already logged. The returned func restores it;
// it also runs on cleanup in case the test fails first.
func unthrottleDataPlaneWarnings(t *testing.T) func() {
	t.Helper()
	orig := dataPlaneWarn
	dataPlaneWarn = plog.NewThrottle(0)
	restore := func() { dataPlaneWarn = orig }
	t.Cleanup(restore)
	return restore
}

// TestIntegration_DataSlotsAutoRegisterRouteAndPrimeLiveness walks the full story: a freshly
// connected idle client registers its route by itself, its heartbeat completes a round trip, a
// port-forward-style teardown re-registers everything, and a client that cannot determine its own
// addresses reproduces the production black hole exactly.
func TestIntegration_DataSlotsAutoRegisterRouteAndPrimeLiveness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := newRegServer(ctx, t)
	v4 := net.ParseIP(regTestTunIPv4).To4()
	v6 := net.ParseIP(regTestTunIPv6).To16()
	stats := startRegClient(ctx, t, srv.port, v4, v6)

	// Phase 1: every data slot announces the TUN IP on connect — no traffic, no manual help.
	// The control slot must NOT appear here: the server deliberately keeps it out of RouteHub.
	waitFor(t, 20*time.Second, func() bool {
		return srv.routeConnCount(v4) == ConnPoolSize
	},
		"all data slots to register the IPv4 route")
	waitFor(t, 20*time.Second, func() bool {
		return srv.routeConnCount(v6) == ConnPoolSize
	},
		"all data slots to register the IPv6 route")
	t.Logf("✅ Phase 1: %d data conns registered for %s and %s (control slot correctly excluded)",
		ConnPoolSize, regTestTunIPv4, regTestTunIPv6)

	// Phase 2: with a route in place the heartbeat completes its round trip — out over the control
	// slot, back over a data conn. This is the signal the liveness watchdog primes on.
	waitFor(t, 20*time.Second, func() bool {
		return !stats.LastReply().IsZero()
	},
		"heartbeat echo reply to be observed")
	t.Logf("✅ Phase 2: heartbeat primed, last reply at %v", stats.LastReply().Format(time.RFC3339Nano))

	// Phase 3: the port-forward is torn down (what the watchdog does every 30s in production).
	// Every slot must reconnect AND re-announce, and liveness must recover on its own.
	before := stats.LastReply()
	preTeardown := srv.routeConns(v4) // the ConnPoolSize conns registered before teardown
	srv.dropAllConns()
	// The client reconnects immediately on a clean disconnect (no backoff — SlotReconnectBackoff
	// only applies on a dial failure), so a fresh conn can register on this route key before the
	// last torn-down conn is evicted, and the aggregate count may never read 0. Assert instead
	// that every pre-teardown conn is gone, which is stable regardless of reconnect timing.
	waitFor(t, 20*time.Second, func() bool {
		remaining := srv.routeConns(v4)
		for _, old := range preTeardown {
			for _, cur := range remaining {
				if old == cur {
					return false
				}
			}
		}
		return true
	},
		"the torn-down conns to be evicted from the route")
	waitFor(t, 30*time.Second, func() bool {
		return srv.routeConnCount(v4) == ConnPoolSize && srv.routeConnCount(v6) == ConnPoolSize
	},
		"all data slots to re-register after reconnect")
	waitFor(t, 30*time.Second, func() bool {
		return stats.LastReply().After(before)
	},
		"heartbeat to recover after reconnect")
	t.Log("✅ Phase 3: routes re-registered and heartbeat recovered after a port-forward teardown")

	// Phase 4: the production failure, reproduced. A client that cannot determine its own
	// addresses announces nothing, so the server has no route, drops every echo reply it
	// generates, and the client never primes — which is what drove the 30s reconnect loop.
	// It must also SAY so: at Debug level this state produced 20 hours of clean logs.
	logs := &syncBuffer{}
	blindCtx := plog.WithLogger(ctx, plog.GetLoggerForClient(int32(logrus.WarnLevel), logs))
	restore := unthrottleDataPlaneWarnings(t)
	routesBefore := srv.routeKeyCount()
	blind := startRegClient(blindCtx, t, srv.port, nil, nil)
	time.Sleep(3 * config.HeartbeatInterval)
	restore()
	if got := srv.routeKeyCount(); got != routesBefore {
		t.Fatalf("blind client added %d route(s) to the hub, want 0", got-routesBefore)
	}
	if !blind.LastReply().IsZero() {
		t.Fatal("blind client observed a heartbeat reply; the route→liveness dependency this test " +
			"pins down no longer holds — re-check the diagnosis before relaxing this assertion")
	}
	// The two diagnostics that were missing when this happened for real.
	for _, want := range []string{"Cannot announce our route", "Heartbeat not sent"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("no %q warning was logged; this failure mode must never be silent again.\nlogs:\n%s",
				want, logs.String())
		}
	}
	t.Log("✅ Phase 4: no addresses ⇒ no route ⇒ every echo reply dropped ⇒ liveness never primes, loudly")
}

// A blocked data stream must not put heartbeat requests behind its queue. This
// deliberately does not assert reply liveness: replies use the stalled data path.
func TestIntegration_SingleDataSlot_ControlBypassesFullQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stats := &HeartbeatStats{}
	dataReady := make(chan struct{})
	resumeData := make(chan struct{})
	controlFrames := make(chan []byte, 16)
	dataFrames := make(chan []byte, 3*MaxSize)
	var peers sync.WaitGroup
	var connectionsMu sync.Mutex
	var connections []net.Conn
	forward := &Forwarder{
		Addr: "pipe", MaxRetries: 1, Connector: NewUDPOverTCPConnector(),
		Transporter: &mockTransporter{dialFn: func(_ context.Context, _ string) (net.Conn, error) {
			client, server := net.Pipe()
			connectionsMu.Lock()
			connections = append(connections, server)
			connectionsMu.Unlock()
			peers.Add(1)
			go func() {
				defer peers.Done()
				defer server.Close()
				framed, _ := NewUDPConnOverTCP(ctx, server)
				buf := make([]byte, config.LargeBufferSize)
				n, err := framed.Read(buf)
				if err != nil || n == 0 {
					return
				}
				control := buf[0] == packetTypeControl
				if !control {
					close(dataReady)
					select {
					case <-resumeData:
					case <-ctx.Done():
						return
					}
				}
				for {
					n, err = framed.Read(buf)
					if err != nil {
						return
					}
					out := dataFrames
					if control {
						out = controlFrames
					}
					select {
					case out <- bytes.Clone(buf[:n]):
					case <-ctx.Done():
						return
					}
				}
			}()
			return client, nil
		}},
	}
	v4 := net.ParseIP(regTestTunIPv4)
	dev := &tunDevice{
		addrsFn:    func() (net.IP, net.IP, net.IP) { return v4, nil, nil },
		tunInbound: make(chan *Packet, MaxSize), tunOutbound: make(chan *Packet, MaxSize),
	}
	ct := newClientTransport(dev, forward, stats)
	var workers sync.WaitGroup
	for _, run := range []func(context.Context){ct.runConnPool, ct.runControlSlot, ct.heartbeats} {
		workers.Add(1)
		go func() { defer workers.Done(); run(ctx) }()
	}
	var producers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		workers.Wait()
		producers.Wait()
		ct.close()
		drainPacketChan(dev.tunInbound)
		drainPacketChan(dev.tunOutbound)
		connectionsMu.Lock()
		for _, c := range connections {
			_ = c.Close()
		}
		connectionsMu.Unlock()
		peers.Wait()
	})
	select {
	case <-dataReady:
	case <-time.After(5 * time.Second):
		t.Fatal("data connection did not register")
	}
	if len(ct.slots) != 1 {
		t.Fatalf("got %d data slots, want 1", len(ct.slots))
	}
	const count = 2*MaxSize + maxBatchFrames + 4
	producers.Add(1)
	go func() {
		defer producers.Done()
		for i := 0; i < count; i++ {
			ip := buildIPv4PacketWithPorts(v4, net.IPv4(10, 0, 0, 5), 6, uint16(20000+i%8), 80)
			pkt := makePoolPacket(ip)
			select {
			case dev.tunInbound <- pkt:
			case <-ctx.Done():
				pkt.release()
				return
			}
		}
	}()
	waitFor(t, 5*time.Second, func() bool {
		return len(ct.slots[0].inbound) == MaxSize && len(dev.tunInbound) == MaxSize
	}, "blocked data writer filled both queues")
	// Consume the initial probe, then demand another while data remains blocked.
	select {
	case <-controlFrames:
	case <-time.After(5 * time.Second):
		t.Fatal("initial control request missing")
	}
	stats.RequestProbe()
	select {
	case frame := <-controlFrames:
		if len(frame) <= typePrefixLen || frame[0] != packetTypeControl {
			t.Fatal("business traffic sent on control connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("full business queue blocked control request")
	}
	if len(ct.slots[0].inbound) != MaxSize {
		t.Fatal("data queue unexpectedly drained during control probe")
	}
	close(resumeData)
	for i := 0; i < count; i++ {
		select {
		case frame := <-dataFrames:
			if len(frame) <= typePrefixLen || frame[0] != packetTypeToGvisor {
				t.Fatal("control traffic entered business queue")
			}
			key := parseFiveTupleInline(frame[typePrefixLen:])
			if !key.hasPorts || key.srcPort != uint16(20000+i%8) {
				t.Fatalf("business packet %d reordered or corrupted", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("business traffic did not resume: received %d/%d packets", i, count)
		}
	}
}

// TestRegistrationPayloadsUseInjectedAddresses pins the contract that registration payloads are
// built from the device's addresses (whatever their source) and are well-formed ICMP echoes to the
// gateway — the packets the server turns into a route.
func TestRegistrationPayloadsUseInjectedAddresses(t *testing.T) {
	v4 := net.ParseIP(regTestTunIPv4).To4()
	v6 := net.ParseIP(regTestTunIPv6).To16()
	dev := &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return v4, v6, nil }}
	ct := &clientTransport{dev: dev}

	payloads := ct.registrationPayloads(context.Background())
	if len(payloads) != 2 {
		t.Fatalf("got %d registration payloads, want 2 (one per address family)", len(payloads))
	}
	for _, p := range payloads {
		if p[0] != packetTypeToGvisor {
			t.Fatalf("payload type prefix = %d, want packetTypeToGvisor (%d)", p[0], packetTypeToGvisor)
		}
		src, dst, _, err := netutil.ParseIPFast(p[typePrefixLen:])
		if err != nil {
			t.Fatalf("payload is not a parseable IP packet: %v", err)
		}
		switch {
		case src.Equal(v4):
			if !dst.Equal(config.RouterIP) {
				t.Fatalf("IPv4 registration dst = %s, want gateway %s", dst, config.RouterIP)
			}
		case src.Equal(v6):
			if !dst.Equal(config.RouterIP6) {
				t.Fatalf("IPv6 registration dst = %s, want gateway %s", dst, config.RouterIP6)
			}
		default:
			t.Fatalf("unexpected registration src %s", src)
		}
	}

	// No addresses ⇒ nothing to announce. This is the state that black-holed production; the
	// throttled warning that now accompanies it is asserted separately.
	blind := &clientTransport{dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return nil, nil, nil }}}
	if got := blind.registrationPayloads(context.Background()); got != nil {
		t.Fatalf("got %d payloads with no addresses, want none", len(got))
	}
}
