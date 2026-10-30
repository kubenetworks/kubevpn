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
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
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
	clientA    *stack.Stack
	appEp      *channel.Endpoint
	sinkPort   uint16
	sinkBytes  *atomic.Int64
	server     interClientServer
	ctx        context.Context
	cancel     context.CancelFunc
	once       sync.Once
	workers    sync.WaitGroup
	mu         sync.Mutex
	closing    bool
	resources  []io.Closer
	transports []*clientTransport
	handler    *gvisorTCPHandler
	stats      [2]*HeartbeatStats
}

// own/goRun cover only fixture-owned resources/tasks, not unexposed library
// workers. Admission is fenced before Wait, including accepts and dials racing stop.
func (h *xclientHarness) own(c io.Closer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		c.Close()
		return false
	}
	h.resources = append(h.resources, c)
	return true
}
func (h *xclientHarness) goRun(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return
	}
	h.workers.Add(1)
	go func() { defer h.workers.Done(); fn() }()
}
func (h *xclientHarness) close() {
	h.once.Do(func() {
		h.mu.Lock()
		h.closing = true
		resources := append([]io.Closer(nil), h.resources...)
		h.mu.Unlock()
		var stacks []*clientStack
		if h.handler != nil {
			h.handler.mu.Lock()
			h.cancel() // fence publication before capturing owned server stacks
			for _, cs := range h.handler.clients {
				stacks = append(stacks, cs)
			}
			h.handler.mu.Unlock()
		} else {
			h.cancel()
		}
		for _, c := range resources {
			c.Close()
		}
		h.workers.Wait()
		for _, cs := range stacks {
			<-cs.done
		}
		if h.clientA != nil {
			h.clientA.Destroy()
			h.appEp.Close()
		}
		for _, ct := range h.transports {
			ct.close()
			drainPacketChan(ct.dev.tunInbound)
			drainPacketChan(ct.dev.tunOutbound)
		}
		h.server.hub.stopTUNBridge()
	})
}

// xclientBenchmarkTransport returns an unwrapped concrete TCP or real TLS conn,
// preserving writev versus aggregate-buffer selection in the real slot writer.
type xclientBenchmarkTransport struct {
	h         *xclientHarness
	encrypted bool
}

func (t xclientBenchmarkTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if !t.h.own(c) {
		return nil, net.ErrClosed
	}
	if !t.encrypted {
		return c, nil
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) // loopback fixture only
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

func (h *xclientHarness) listen(b *testing.B, handle func(net.Conn)) net.Listener {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	h.own(ln)
	h.goRun(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if !h.own(c) {
				return
			}
			h.goRun(func() { defer c.Close(); handle(c) })
		}
	})
	return ln
}

// The legacy entrypoint retains its topology: B runs full routines; A runs the
// data pool plus its originator bridge. New modes vary only stats and transport.
// latency is a per-read shaping artifact, NOT WAN RTT; new modes always use zero.
func newXClientHarness(b *testing.B, latency time.Duration) *xclientHarness {
	return newXClientHarnessMode(b, latency, false, false, true)
}
func newXClientHarnessMode(b *testing.B, latency time.Duration, encrypted, statsOn, legacy bool) *xclientHarness {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &xclientHarness{ctx: ctx, cancel: cancel, sinkBytes: &atomic.Int64{}, server: interClientServer{hub: NewRouteHub()}}
	b.Cleanup(h.close) // also covers setup failures
	if statsOn {
		h.stats = [2]*HeartbeatStats{{}, {}}
	} else if legacy {
		h.stats[1] = &HeartbeatStats{} // preserve the existing B fixture
	}
	sinkLn := h.listen(b, func(c net.Conn) {
		buf := make([]byte, 64*1024)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				h.sinkBytes.Add(int64(n))
			}
			if err != nil {
				return
			}
		}
	})
	h.sinkPort = uint16(sinkLn.Addr().(*net.TCPAddr).Port)
	h.handler = GvisorLocalTCPHandler(h.server.hub).(*gvisorTCPHandler)
	var tlsConfig *tls.Config
	if encrypted {
		tlsConfig = batchTLSConfig(b)
	}
	ln := h.listen(b, func(c net.Conn) {
		if encrypted {
			tc := tls.Server(c, tlsConfig)
			if tc.HandshakeContext(ctx) != nil {
				return
			}
			c = tc
		}
		if latency > 0 {
			c = &shapedConn{Conn: c, latency: latency, done: make(chan struct{})}
		}
		h.handler.Handle(ctx, c)
	})
	h.server.port = ln.Addr().(*net.TCPAddr).Port
	forward := &Forwarder{Addr: ln.Addr().String(), Connector: NewUDPOverTCPConnector(), Transporter: xclientBenchmarkTransport{h, encrypted}, MaxRetries: 3}

	osB, tunB := net.Pipe()
	h.own(osB)
	h.own(tunB)
	deviceB := h.newDevice(tunB, forward, h.stats[1])
	for _, r := range deviceB.routines() {
		h.goRun(func() { r.fn(ctx) })
	}
	h.goRun(func() { _, _ = io.Copy(io.Discard, osB) })
	registerRoute(b, osB, xclientBIP)
	waitForRoutes(b, h.server.hub, xclientBIP)

	tunEnd, appEnd := newPacketConnPair()
	// packetConn endpoints share done but not sync.Once; closing this endpoint
	// shuts down both sides. Do not close its peer a second time.
	h.own(tunEnd)
	deviceA := h.newDevice(tunEnd, forward, h.stats[0])
	ct := deviceA.transport.(*clientTransport)
	h.goRun(func() { deviceA.readFromTun(ctx) })
	h.goRun(func() { deviceA.writeToTun(ctx) })
	h.goRun(func() { ct.runConnPool(ctx) })
	h.appEp = channel.New(8192, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	h.appEp.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	h.clientA = newGvisorStack(ctx, h.appEp, LocalTCPForwarder, LocalUDPForwarder)
	ip := net.ParseIP(xclientAIP).To4()
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4([4]byte(ip)).WithPrefix()}
	if err := h.clientA.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
		b.Fatalf("add client A addr: %v", err)
	}
	h.goRun(func() { bridgeEndpointToConn(ctx, h.appEp, appEnd) })
	h.goRun(func() { bridgeConnToEndpoint(ctx, appEnd, h.appEp) })
	time.Sleep(500 * time.Millisecond) // unchanged legacy warmup, outside measurement
	return h
}
func (h *xclientHarness) newDevice(tun net.Conn, forward *Forwarder, stats *HeartbeatStats) *tunDevice {
	d := &tunDevice{tun: tun, tunInbound: make(chan *Packet, MaxSize), tunOutbound: make(chan *Packet, MaxSize), errChan: make(chan error, 1)}
	ct := newClientTransport(d, forward, stats)
	d.transport = ct
	h.transports = append(h.transports, ct)
	return d
}

// runFlows dials `flows` concurrent TCP flows from A to dstIP:sinkPort, pumps a total of b.N
// chunks across them, and measures the wall time until the sink has received everything.
// Reports aggregate MB/s (via b.SetBytes). Both clients use the production default
// of one data slot. Varying business flows does not vary the tunnel connection count;
// docs/50's earlier batching results used the previous four-data-slot default.
func (h *xclientHarness) runFlows(b *testing.B, dstIP string, chunkSize, flows int) {
	ctx, cancel := context.WithTimeout(h.ctx, 60*time.Second)
	defer cancel()
	ip := net.ParseIP(dstIP).To4()
	remote := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte(ip)), Port: h.sinkPort}
	conns := make([]*gonet.TCPConn, flows)
	for i := range conns {
		conn, err := gonet.DialContextTCP(ctx, h.clientA, remote, ipv4.ProtocolNumber)
		if err != nil {
			b.Fatalf("dial %d A->%s: %v", i, dstIP, err)
		}
		conns[i] = conn
		h.own(conn)
		_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
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

	if h.sinkBytes.Load()-startSink < total {
		for _, c := range conns {
			c.Close()
		}
	}
	var firstErr error
	for range conns {
		if err := <-writeErr; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		b.Fatalf("write flow A->%s: %v", dstIP, firstErr)
	}
	if got := h.sinkBytes.Load() - startSink; got != total {
		b.Fatalf("A->%s (%d flows): sink received %d of %d bytes (stalled / lost)", dstIP, flows, got, total)
	}
}

// BenchmarkCrossClientMixed reuses the production cross-client harness without
// changing the baseline benchmark. Seven bulk flows compete with a serialized
// request/echo flow; percentiles are actual round-trip samples, not write latency.
func BenchmarkCrossClientMixed(b *testing.B) {
	silenceDataPlaneLogs()
	h := newXClientHarness(b, 0)
	defer h.close()
	h.runMixed(b)
}

func BenchmarkCrossClientMixedModes(b *testing.B) {
	silenceDataPlaneLogs()
	for _, encrypted := range []bool{false, true} {
		transport := "rawTCP"
		if encrypted {
			transport = "TLS"
		}
		for _, enabled := range []bool{false, true} {
			statsMode := "off"
			if enabled {
				statsMode = "on"
			}
			b.Run(transport+"/stats="+statsMode, func(b *testing.B) {
				h := newXClientHarnessMode(b, 0, encrypted, enabled, false)
				defer h.close()
				b.ReportAllocs()
				h.runMixed(b)
			})
		}
	}
}

func (h *xclientHarness) runMixed(b *testing.B) {
	ln := h.listen(b, func(c net.Conn) { _, _ = io.Copy(c, c) })
	addr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(net.ParseIP(xclientBIP).To4()), Port: uint16(ln.Addr().(*net.TCPAddr).Port)}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	request, err := gonet.DialContextTCP(ctx, h.clientA, addr, ipv4.ProtocolNumber)
	if err != nil {
		b.Fatal(err)
	}
	h.own(request)
	defer request.Close()
	bulkAddr := addr
	bulkAddr.Port = h.sinkPort
	bulk := make([]*gonet.TCPConn, 7)
	done := make(chan struct{}, len(bulk))
	for i := range bulk {
		bulk[i], err = gonet.DialContextTCP(ctx, h.clientA, bulkAddr, ipv4.ProtocolNumber)
		if err != nil {
			b.Fatal(err)
		}
		h.own(bulk[i])
	}
	defer func() {
		cancel() // also releases workers still waiting at the start gate
		for _, c := range bulk {
			c.Close()
		}
		for range bulk {
			<-done
		}
	}()
	startBulk := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(len(bulk))
	for _, c := range bulk {
		go func(c *gonet.TCPConn) {
			defer func() { done <- struct{}{} }()
			data := make([]byte, 32*1024)
			ready.Done()
			select {
			case <-startBulk:
			case <-ctx.Done():
				return
			}
			for {
				if _, e := c.Write(data); e != nil {
					return
				}
			}
		}(c)
	}
	samples := make([]time.Duration, b.N)
	payload := make([]byte, 64)
	reply := make([]byte, 64)
	ready.Wait()
	startBytes := h.sinkBytes.Load()
	b.ResetTimer()
	start := time.Now()
	close(startBulk) // no bulk payload was submitted before this window
	for i := range samples {
		_ = request.SetDeadline(time.Now().Add(5 * time.Second))
		sent := time.Now()
		if _, err := request.Write(payload); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(request, reply); err != nil {
			b.Fatal(err)
		}
		samples[i] = time.Since(sent)
	}
	elapsed := time.Since(start)
	endBytes := h.sinkBytes.Load()
	b.StopTimer()
	slices.Sort(samples)
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Nanoseconds())/1000, "request-p95-us")
		b.ReportMetric(float64(samples[(len(samples)-1)*99/100].Nanoseconds())/1000, "request-p99-us")
	}
	b.ReportMetric(float64(endBytes-startBytes)/elapsed.Seconds()/1e6, "bulk-MB/s")
}

// BenchmarkFramedBatchWrites measures deterministic writes per frame over a real
// wrapped TCP socket; the wrapper deliberately takes the aggregation (TLS) path.
func BenchmarkFramedBatchWrites(b *testing.B) {
	raw, peer := batchTCPPair(b, false)
	conn := &batchTestConn{Conn: raw, omitSizes: true}
	drained := make(chan struct{})
	go func() { io.Copy(io.Discard, peer); close(drained) }()
	defer func() { raw.Close(); <-drained }()
	q := make(chan *Packet, 32)
	var packets []*Packet
	for i := 0; i < 32; i++ {
		p := batchPacket(1500, i)
		p.release()
		packets = append(packets, p)
	}
	defer func() {
		for _, p := range packets {
			p.release()
		}
	}()
	var batch packetBatch
	var writer frameBatchWriter
	b.SetBytes(32 * 1500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range packets {
			p.acquire()
			q <- p
		}
		if !batch.next(context.Background(), q, 32, nil) {
			b.Fatal("empty burst")
		}
		_, err := writer.write(conn, batch.packets[:batch.n])
		batch.release()
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(conn.calls)/float64(b.N*32), "writes/frame")
}

// BenchmarkCrossClientModes is the step-0 stats/transport comparison, restricted
// to latency=0 and the current one-data-slot topology. Legacy names remain below.
func BenchmarkCrossClientModes(b *testing.B) {
	silenceDataPlaneLogs()
	for _, encrypted := range []bool{false, true} {
		transport := "rawTCP"
		if encrypted {
			transport = "TLS"
		}
		for _, enabled := range []bool{false, true} {
			statsMode := "off"
			if enabled {
				statsMode = "on"
			}
			for _, path := range []string{"crossclient", "cluster"} {
				for _, flows := range []int{1, 8} {
					b.Run(fmt.Sprintf("%s/stats=%s/%s/flows=%d", transport, statsMode, path, flows), func(b *testing.B) {
						h := newXClientHarnessMode(b, 0, encrypted, enabled, false)
						defer h.close()
						dst := xclientBIP
						if path == "cluster" {
							dst = xclientCIP
						}
						b.ReportAllocs()
						h.runFlows(b, dst, 32*1024, flows)
						if enabled && h.stats[0].Snapshot().Slots[0].WritePackets == 0 {
							b.Fatal("stats mode recorded no data")
						}
					})
				}
			}
		}
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
