package core

// Cross-client (A->B) throughput benchmark — also the regression guard for the docs/50 fix.
//
// Symptom that motivated it: two kubevpn clients, A accessing a service on B's machine, was slow
// on BOTH transports (kubectl port-forward AND the SSH data-plane bypass). Since SSH has proper
// TCP window scaling and no SPDY/apiserver proxy, "both slow" pointed away from the transport and
// at the path the two modes SHARE: the server RouteHub hairpin plus B's inter-client gvisor stack.
// This benchmark wires that exact path end-to-end WITHOUT a TUN device or cluster (no root, runs
// on arm64):
//
//	A: gonet TCP  → A's gvisor stack ⇄ packetConn (= TUN) → A's connection pool
//	   → real loopback TCP tunnel → server (RouteHub + GvisorLocalTCPHandler)
//	     ├─ crossclient: HasRoute(B) → WriteToRoutePacket → B's pool
//	     │    → B.connSlot.readFromConn → interClientStack.InjectIP (direct, TCP-window flow control)
//	     │      → B's inter-client gvisor stack → LocalTCPForwarder → 127.0.0.1:<sink>
//	     └─ cluster:    inject into server's own gvisor stack → LocalTCPForwarder → 127.0.0.1:<sink>
//
// Comparing "crossclient" vs "cluster" isolates the cross-client-specific stage from the shared
// transport+forward cost. Before the fix (a bounded drop-if-full Go channel in front of B's stack),
// crossclient throughput FELL as concurrency rose (~180→~90 MB/s at 8 flows) while cluster scaled
// up (~250→~360), and B silently dropped thousands of inbound segments. After switching B to inject
// directly into the shared stack (docs/50), crossclient should track cluster far more closely and
// no longer regress with concurrency.
//
// Run:
//	go test ./pkg/core/ -run '^$' -bench BenchmarkCrossClient -benchmem -benchtime=2s -count=3

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	logrus "github.com/sirupsen/logrus"
	glog "gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	xclientAIP = "198.18.0.2" // client A's TUN IP (originator)
	xclientBIP = "198.18.0.3" // client B's TUN IP (receiver, runs the inter-client stack)
	xclientCIP = "10.96.0.5"  // a stand-in "cluster" IP: server-side stack forwards it to the sink
)

// silenceDataPlaneLogs mutes gvisor's per-packet sniffer and kubevpn's per-conn logs so they
// neither dominate CPU nor flood benchmark output (which would invalidate throughput numbers).
func silenceDataPlaneLogs() {
	sniffer.LogPackets.Store(0)
	glog.SetLevel(glog.Warning)
	plog.L.SetLevel(logrus.FatalLevel)
}

type xclientHarness struct {
	clientA   *stack.Stack
	sinkPort  uint16
	sinkBytes *atomic.Int64
	server    interClientServer
	cancel    context.CancelFunc
}

// newShapedInterClientServer is newInterClientServer with an optional per-conn read latency on
// every accepted tunnel connection, modelling the client<->traffic-manager RTT. Both A's and B's
// pool connections are accepted here, so the shaping applies symmetrically to both client hops
// (matching how connpool_bench_test.go models a latency link).
func newShapedInterClientServer(ctx context.Context, b *testing.B, latency time.Duration) interClientServer {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("server listener: %v", err)
	}
	b.Cleanup(func() { ln.Close() })
	hub := NewRouteHub()
	handler := GvisorLocalTCPHandler(hub)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if latency > 0 {
				conn = &shapedConn{Conn: conn, latency: latency}
			}
			go handler.Handle(ctx, conn)
		}
	}()
	return interClientServer{hub: hub, port: ln.Addr().(*net.TCPAddr).Port}
}

// newXClientHarness wires server + a registered client B + originator client A + a shared loopback
// sink. latency shapes each client<->server hop (0 = clean loopback). Returns once B's route is
// registered and A's connection pool has had time to connect.
func newXClientHarness(b *testing.B, latency time.Duration) *xclientHarness {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	// Shared sink: both B's inter-client forwarder and the server's cluster forwarder dial
	// 127.0.0.1:<sinkPort>. The two sub-benchmarks never run concurrently, so one sink suffices.
	sinkLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		b.Fatalf("sink listen: %v", err)
	}
	go func() { <-ctx.Done(); sinkLn.Close() }()
	sinkBytes := &atomic.Int64{}
	go func() {
		for {
			c, err := sinkLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64*1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						sinkBytes.Add(int64(n))
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	sinkPort := uint16(sinkLn.Addr().(*net.TCPAddr).Port)

	// Tunnel server: RouteHub + GvisorLocalTCPHandler (forwards injected traffic to 127.0.0.1),
	// with an optional per-hop RTT shaping the accepted client connections.
	server := newShapedInterClientServer(ctx, b, latency)

	// Client B: full clientTransport. Its inter-client stack routine forwards A->B traffic to
	// 127.0.0.1:<port A dialed> = the sink. Register B's route so the server hairpins to it
	// (an unregistered TUN-pool dst would be dropped, see readFromTCPConnWriteToEndpoint).
	osB, _ := startPipeClient(ctx, b, server.port, xclientBIP)
	registerRoute(b, osB, xclientBIP)
	waitForRoutes(b, server.hub, xclientBIP)

	// Client A: a gvisor stack that originates flows, bridged through a real tunDevice + pool.
	clientA := buildOriginatorStack(ctx, b, server.port, xclientAIP)
	// Let A's connection pool establish before flows are dialed (A registers its own route
	// lazily on the first data packet; the gonet handshake tolerates the brief warm-up).
	time.Sleep(500 * time.Millisecond)

	return &xclientHarness{clientA: clientA, sinkPort: sinkPort, sinkBytes: sinkBytes, server: server, cancel: cancel}
}

func (h *xclientHarness) close() { h.cancel() }

// buildOriginatorStack mirrors production client A: gonet/kernel TCP -> gvisor stack -> TUN ->
// connection pool -> tunnel server. Flows are dialed from the returned stack with gonet.
func buildOriginatorStack(ctx context.Context, b *testing.B, serverPort int, tunIP string) *stack.Stack {
	b.Helper()
	tunEnd, appEnd := newPacketConnPair()
	device := &tunDevice{
		tun:         tunEnd,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	ct := newClientTransport(device, &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", serverPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}, nil)
	device.transport = ct
	// Only the data-plane routines are needed on A (no inter-client stack, no heartbeat: the
	// route is registered by the first data packet's AddRoute on the server).
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go ct.runConnPool(ctx)

	appEp := channel.New(8192, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	appEp.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	s := newGvisorStack(ctx, appEp, LocalTCPForwarder, LocalUDPForwarder)
	ip := net.ParseIP(tunIP).To4()
	addr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte(ip)).WithPrefix(),
	}
	if err := s.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
		b.Fatalf("add client A addr: %v", err)
	}
	go bridgeEndpointToConn(ctx, appEp, appEnd)
	go bridgeConnToEndpoint(ctx, appEnd, appEp)
	return s
}

// runFlows dials `flows` concurrent TCP flows from A to dstIP:sinkPort, pumps a total of b.N
// chunks across them, and measures the wall time until the sink has received everything. Reports
// aggregate MB/s (via b.SetBytes) and the inter-client drop delta over the run.
//
// The flows dimension probes two structural hypotheses:
//   - Single flow (proto,dst,srcPort,dstPort five-tuple) is pinned to ONE of A's 4 pool slots, so
//     it cannot use pool parallelism — expect ~no gain from the pool on one flow.
//   - Cross-client traffic to B, regardless of flow count, funnels through B's SINGLE shared
//     inter-client gvisor stack — a potential serialization point that concurrency cannot widen,
//     unlike the server's per-source cluster stacks.
func (h *xclientHarness) runFlows(b *testing.B, dstIP string, chunkSize, flows int) {
	ctx := context.Background()
	ip := net.ParseIP(dstIP).To4()
	remote := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte(ip)), Port: h.sinkPort}
	conns := make([]*gonet.TCPConn, flows)
	for i := range conns {
		conn, err := gonet.DialContextTCP(ctx, h.clientA, remote, ipv4.ProtocolNumber)
		if err != nil {
			b.Fatalf("dial %d A->%s: %v", i, dstIP, err)
		}
		conns[i] = conn
		defer conn.Close()
	}

	chunk := make([]byte, chunkSize)
	startSink := h.sinkBytes.Load()
	total := int64(b.N) * int64(chunkSize)
	b.SetBytes(int64(chunkSize))
	b.ResetTimer()

	var remaining atomic.Int64
	remaining.Store(int64(b.N))
	writeErr := make(chan error, flows)
	for _, conn := range conns {
		go func(c *gonet.TCPConn) {
			for remaining.Add(-1) >= 0 {
				if _, err := c.Write(chunk); err != nil {
					writeErr <- err
					return
				}
			}
			writeErr <- nil
		}(conn)
	}

	deadline := time.Now().Add(60 * time.Second)
	for h.sinkBytes.Load()-startSink < total && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	b.StopTimer()

	for range conns {
		if err := <-writeErr; err != nil {
			b.Fatalf("write flow A->%s: %v", dstIP, err)
		}
	}
	if got := h.sinkBytes.Load() - startSink; got < total {
		b.Fatalf("A->%s (%d flows): sink received %d of %d bytes (stalled / lost)", dstIP, flows, got, total)
	}
}

// BenchmarkCrossClientThroughput compares throughput of A->B (cross-client: exercises B's single
// shared inter-client gvisor stack + the drop-if-full receive path) against A->cluster (exercises
// the server's per-source forward stack only), at 1 and 8 concurrent flows. Both run over an
// identical clean loopback transport, so any gap is intrinsic to the cross-client path — which is
// what makes "both port-forward and SSH are slow" consistent: the shared stage, not the transport.
//
// A faithful high-RTT / BDP dimension is deliberately NOT modeled here: shapedConn adds a uniform
// per-read sleep that penalizes a single flow far beyond a real RTT and collapses both paths
// equally (a modeling artifact, not a signal). Real-RTT throughput is measured in the CI e2e job.
func BenchmarkCrossClientThroughput(b *testing.B) {
	silenceDataPlaneLogs()
	const chunkSize = 32 * 1024

	h := newXClientHarness(b, 0)
	defer h.close()

	for _, flows := range []int{1, 8} {
		b.Run(fmt.Sprintf("crossclient/flows=%d", flows), func(b *testing.B) { h.runFlows(b, xclientBIP, chunkSize, flows) })
		b.Run(fmt.Sprintf("cluster/flows=%d", flows), func(b *testing.B) { h.runFlows(b, xclientCIP, chunkSize, flows) })
	}
}
