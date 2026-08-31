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
//	client tunDevice (net.Pipe as TUN) + clientTransport (4 data slots + 1 control slot)
//	  → loopback TCP → server gvisorTCPHandler + RouteHub
//	    → per-client gvisor stack answers the gateway echo
//	      → RouteHub routes the reply back over a DATA conn → client marks HeartbeatStats
//
// Note the existing inter-client ICMP e2e test has to call registerRoute() by hand — it was
// working around exactly this bug.

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
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
	key := regRouteKey(ip)
	val, ok := s.hub.RouteMapTCP.Load(key)
	if !ok {
		return 0
	}
	return val.(*ConnList).Len()
}

// routeKeyCount returns how many distinct addresses the hub currently has a route for.
func (s *regServer) routeKeyCount() int {
	n := 0
	s.hub.RouteMapTCP.Range(func(_, _ any) bool { n++; return true })
	return n
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
	srv.dropAllConns()
	waitFor(t, 20*time.Second, func() bool {
		return srv.routeConnCount(v4) == 0
	},
		"routes to be dropped after teardown")
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
	routesBefore := srv.routeKeyCount()
	blind := startRegClient(ctx, t, srv.port, nil, nil)
	time.Sleep(3 * config.HeartbeatInterval)
	if got := srv.routeKeyCount(); got != routesBefore {
		t.Fatalf("blind client added %d route(s) to the hub, want 0", got-routesBefore)
	}
	if !blind.LastReply().IsZero() {
		t.Fatal("blind client observed a heartbeat reply; the route→liveness dependency this test " +
			"pins down no longer holds — re-check the diagnosis before relaxing this assertion")
	}
	t.Log("✅ Phase 4: no addresses ⇒ no route ⇒ every echo reply dropped ⇒ liveness never primes")
}

// TestRegistrationPayloadsUseInjectedAddresses pins the contract that registration payloads are
// built from the device's addresses (whatever their source) and are well-formed ICMP echoes to the
// gateway — the packets the server turns into a route.
func TestRegistrationPayloadsUseInjectedAddresses(t *testing.T) {
	v4 := net.ParseIP(regTestTunIPv4).To4()
	v6 := net.ParseIP(regTestTunIPv6).To16()
	dev := &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return v4, v6, nil }}
	ct := &clientTransport{dev: dev}

	payloads := ct.registrationPayloads()
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
	if got := blind.registrationPayloads(); got != nil {
		t.Fatalf("got %d payloads with no addresses, want none", len(got))
	}
}
