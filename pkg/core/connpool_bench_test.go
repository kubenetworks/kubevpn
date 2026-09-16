package core

// Benchmark comparing tunnel throughput for different connection-pool sizes
// (ConnPoolSize 1 vs 2/4/8) over a clean and an impaired (added-latency) link.
//
// Why this exists: tun_client.go documents ConnPoolSize=4 as "reduce head-of-line
// blocking and improve throughput", but nothing measured it. This wires the REAL
// data plane end-to-end WITHOUT a TUN device (no root):
//
//	bench app (gonet, many dst IPs) → client gvisor stack
//	  ⇄ raw-IP packetConn (= tunDevice.tun)
//	    → real tunDevice + clientTransport connection pool (poolSize=N)
//	      → loopback TCP (optionally latency-shaped) → server listener
//	        → GvisorLocalTCPHandler (one gvisor stack per conn) → LocalTCPForwarder
//	          → 127.0.0.1:<sink> (drains, counts bytes)
//
// The pool partitions flows by ipHash(dst), so a SINGLE flow always uses ONE slot:
// pool size can only matter with MANY concurrent flows.
//
// Run:
//	go test ./pkg/core/ -run '^$' -bench BenchmarkConnPool -benchmem -benchtime=2s -count=3
//
// Measured (arm64, -benchtime=1s -count=3, MB/s averaged over the 3 runs):
//
//	                 pool=1  pool=2  pool=4  pool=8
//	clean/flows=16      338     488     502     525
//	clean/flows=64      256     381     445     451
//	clean/flows=1       342     272     292     282   (no trend: one flow pins to one slot)
//	lat2ms/flows=16     391     396     380     393   (flat)
//	lat2ms/flows=64     315     353     363     347
//
// Two conclusions, the second of which contradicts what this file used to assume:
//
//  1. The pool is worth having: at 16 concurrent flows pool=4 is ~+49% over a single
//     conn, at 64 flows ~+74%, and the trend is monotonic. ConnPoolSize=4 sits just
//     past the knee (pool=8 buys little).
//
//  2. That gain comes from the CLEAN link, not the latency-shaped one — the opposite
//     of the original premise that "the pool's value shows on a latency-shaped link
//     where one conn is round-trip-limited". With a correct propagation-delay model
//     (see shapedConn) latency is not a bottleneck for bulk transfer at all and the
//     pool makes almost no difference there. What the pool actually parallelises is
//     OUR OWN per-conn serialization: datagram framing, the per-slot inbound channel
//     and the server's per-conn read loop are each a single-threaded pipeline, so N
//     conns give N pipelines. That is also why the gain does not depend on whether the
//     conns are multiplexed onto one apiserver SPDY session in production.

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	logrus "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/buffer"
	glog "gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/sniffer"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ---------------------------------------------------------------------------
// packetConn: an in-memory, packet-preserving net.Conn pair used in place of a
// real TUN device. Each Write delivers exactly one packet to the peer's Read
// (net.Pipe does not guarantee message boundaries, which would corrupt IP framing).
// ---------------------------------------------------------------------------

type packetConn struct {
	rd     <-chan []byte
	wr     chan<- []byte
	done   chan struct{}
	closeO sync.Once
}

func newPacketConnPair() (*packetConn, *packetConn) {
	a2b := make(chan []byte, 4096)
	b2a := make(chan []byte, 4096)
	done := make(chan struct{})
	a := &packetConn{rd: b2a, wr: a2b, done: done}
	b := &packetConn{rd: a2b, wr: b2a, done: done}
	return a, b
}

func (c *packetConn) Read(p []byte) (int, error) {
	select {
	case pkt := <-c.rd:
		return copy(p, pkt), nil
	case <-c.done:
		return 0, io.EOF
	}
}

func (c *packetConn) Write(p []byte) (int, error) {
	// Copy: the caller (tunDevice/gvisor) reuses its buffer after Write returns.
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case c.wr <- buf:
		return len(p), nil
	case <-c.done:
		return 0, io.ErrClosedPipe
	}
}

func (c *packetConn) Close() error                     { c.closeO.Do(func() { close(c.done) }); return nil }
func (c *packetConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *packetConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *packetConn) SetDeadline(time.Time) error      { return nil }
func (c *packetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *packetConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "packetconn" }
func (dummyAddr) String() string  { return "packetconn" }

// ---------------------------------------------------------------------------
// shapedConn: wraps a net.Conn and delays DELIVERY of inbound bytes by a fixed
// one-way propagation latency, so the tunnelled flows see a round trip of roughly
// that latency.
//
// It must pipeline, and an earlier version did not: it slept for `latency` after
// every Read that returned data, on the reading goroutine. That is not a
// propagation delay, it is a per-read RATE CAP of 1/latency reads per second, and
// it dominated everything else in the measurement — the lat2ms column read
// 0.12–0.33 MB/s against 222–480 MB/s on the clean link, and with a single flow
// (which pins to one slot, so pool size cannot matter) every pool size returned
// the same ~0.19 MB/s, the signature of a fixed per-conn ceiling rather than of
// anything the code under test was doing. Numbers from that column said nothing
// about kubevpn.
//
// Here a background pump keeps draining the underlying conn and stamps each chunk
// with its due time on arrival, so chunk N+1's delay overlaps chunk N's instead of
// queueing behind it. Reads block only until the next chunk is due, which in steady
// state is never: latency then costs a constant start-up delay, exactly as a real
// link does. Bandwidth is deliberately NOT capped — doing so per conn would
// multiply the ceiling by the conn count and manufacture the very speed-up this
// benchmark is meant to measure.
// ---------------------------------------------------------------------------

// shapedChunk is bytes (or a terminal error) that became visible at `due`.
type shapedChunk struct {
	data []byte
	err  error
	due  time.Time
}

type shapedConn struct {
	net.Conn
	latency time.Duration

	chunks    chan shapedChunk
	pending   []byte // remainder of the chunk currently being handed to Read
	done      chan struct{}
	closeOnce sync.Once
}

func newShapedConn(c net.Conn, latency time.Duration) *shapedConn {
	s := &shapedConn{
		Conn:    c,
		latency: latency,
		// Deep enough that the pump is throttled by the underlying conn rather than by the
		// consumer, which is what keeps delays overlapping.
		chunks: make(chan shapedChunk, 1024),
		done:   make(chan struct{}),
	}
	go s.pump()
	return s
}

// pump drains the underlying conn as fast as it will go, stamping each chunk with the time it
// becomes deliverable. One allocation per read: the buffer is handed off to the chunk.
func (s *shapedConn) pump() {
	defer close(s.chunks)
	for {
		buf := make([]byte, 32*1024)
		n, err := s.Conn.Read(buf)
		due := time.Now().Add(s.latency)
		if n > 0 {
			select {
			case s.chunks <- shapedChunk{data: buf[:n], due: due}:
			case <-s.done:
				return
			}
		}
		if err != nil {
			select {
			case s.chunks <- shapedChunk{err: err, due: due}:
			case <-s.done:
			}
			return
		}
	}
}

func (s *shapedConn) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		chunk, ok := <-s.chunks
		if !ok {
			return 0, io.EOF
		}
		if wait := time.Until(chunk.due); wait > 0 {
			time.Sleep(wait)
		}
		if chunk.err != nil {
			return 0, chunk.err
		}
		s.pending = chunk.data
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *shapedConn) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	return s.Conn.Close()
}

// ---------------------------------------------------------------------------
// raw IP bridges between a gvisor channel endpoint and a packetConn end.
// tunDevice.tun carries raw IP packets (one per Read/Write), matching pumpTun/writeToTun.
// ---------------------------------------------------------------------------

func bridgeEndpointToConn(ctx context.Context, ep *channel.Endpoint, conn net.Conn) {
	for ctx.Err() == nil {
		pkt := ep.ReadContext(ctx)
		if pkt == nil {
			return
		}
		ip := pkt.ToView().AsSlice()
		pkt.DecRef()
		if _, err := conn.Write(ip); err != nil {
			return
		}
	}
}

func bridgeConnToEndpoint(ctx context.Context, conn net.Conn, ep *channel.Endpoint) {
	buf := make([]byte, config.DefaultMTU+64)
	for ctx.Err() == nil {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if n < 1 {
			continue
		}
		var proto tcpip.NetworkProtocolNumber
		if buf[0]>>4 == 4 {
			proto = header.IPv4ProtocolNumber
		} else {
			proto = header.IPv6ProtocolNumber
		}
		pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(buf[:n])})
		ep.InjectInbound(proto, pb)
		pb.DecRef()
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type benchHarness struct {
	clientStack *stack.Stack
	sinkPort    uint16
	sinkBytes   *atomic.Int64
	cancel      context.CancelFunc
	device      *tunDevice
}

// newBenchHarness wires the full no-root data path with the given pool size and
// per-conn link latency (0 = clean). Returns once the pool's N connections are up.
func newBenchHarness(b *testing.B, poolSize int, latency time.Duration) *benchHarness {
	b.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	// --- sink: drains and counts bytes (the "k8s service") ---
	sinkLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		b.Fatalf("sink listen: %v", err)
	}
	sinkBytes := &atomic.Int64{}
	go func() {
		<-ctx.Done()
		sinkLn.Close()
	}()
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

	// --- server tunnel side: gvisor handler per accepted pool conn ---
	hub := NewRouteHub()
	serverHandler := GvisorLocalTCPHandler(hub)
	srvLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		b.Fatalf("server listen: %v", err)
	}
	go func() {
		<-ctx.Done()
		srvLn.Close()
	}()
	var srvConns atomic.Int32
	go func() {
		for {
			c, err := srvLn.Accept()
			if err != nil {
				return
			}
			srvConns.Add(1)
			if latency > 0 {
				c = newShapedConn(c, latency)
			}
			go serverHandler.Handle(ctx, c)
		}
	}()
	srvPort := srvLn.Addr().(*net.TCPAddr).Port

	// --- client tunnel side: real tunDevice + connection pool ---
	tunEnd, appEnd := newPacketConnPair()
	device := &tunDevice{
		tun:         tunEnd,
		tunInbound:  make(chan *Packet, MaxSize),
		tunOutbound: make(chan *Packet, MaxSize),
		errChan:     make(chan error, 1),
	}
	ct := newClientTransport(device, &Forwarder{
		Addr:        fmt.Sprintf("127.0.0.1:%d", srvPort),
		Connector:   NewUDPOverTCPConnector(),
		Transporter: TCPTransporter(nil),
		MaxRetries:  3,
	}, nil)
	ct.poolSize = poolSize
	device.transport = ct
	// Start only the data-plane routines (skip heartbeat: it needs a real TUN
	// interface lookup; routes are registered by data packets' AddRoute anyway).
	go device.readFromTun(ctx)
	go device.writeToTun(ctx)
	go ct.runConnPool(ctx)

	// --- client app side: a gvisor stack that originates the TCP flows ---
	appEp := channel.New(8192, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	appEp.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	clientStack := newGvisorStack(ctx, appEp, LocalTCPForwarder, LocalUDPForwarder)
	addr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{198, 18, 0, 2}).WithPrefix(),
	}
	if err := clientStack.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
		cancel()
		b.Fatalf("add client addr: %v", err)
	}
	go bridgeEndpointToConn(ctx, appEp, appEnd)
	go bridgeConnToEndpoint(ctx, appEnd, appEp)

	// wait for the pool to establish N server connections
	deadline := time.Now().Add(10 * time.Second)
	for srvConns.Load() < int32(poolSize) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := srvConns.Load(); got < int32(poolSize) {
		cancel()
		b.Fatalf("pool not established: got %d server conns, want %d", got, poolSize)
	}

	return &benchHarness{clientStack: clientStack, sinkPort: sinkPort, sinkBytes: sinkBytes, cancel: cancel, device: device}
}

// dial opens one app TCP flow to a distinct dst IP (10.96.0.<idx>) so ipHash spreads
// flows across pool slots. The server's LocalTCPForwarder routes every flow to the sink.
func (h *benchHarness) dial(ctx context.Context, idx int) (net.Conn, error) {
	remote := tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4([4]byte{10, 96, byte(idx >> 8), byte(idx)}),
		Port: h.sinkPort,
	}
	return gonet.DialContextTCP(ctx, h.clientStack, remote, ipv4.ProtocolNumber)
}

func (h *benchHarness) close() { h.cancel() }

// ---------------------------------------------------------------------------
// benchmark
// ---------------------------------------------------------------------------

func BenchmarkConnPool(b *testing.B) {
	// Silence gvisor's per-packet sniffer logging: it dominates CPU and floods output,
	// which would invalidate the throughput numbers.
	sniffer.LogPackets.Store(0)
	// The data path also calls sniffer.LogPacket directly (ungated), which logs via
	// gvisor's glog at Info — raise the glog level to drop those per-packet lines.
	glog.SetLevel(glog.Warning)
	// Silence kubevpn's data-plane logs (per-conn setup/teardown) so they don't
	// interleave with benchmark output on stdout.
	plog.L.SetLevel(logrus.FatalLevel)

	const chunkSize = 32 * 1024
	chunk := make([]byte, chunkSize)
	for _, pool := range []int{1, 2, 4, 8} {
		for _, link := range []struct {
			name    string
			latency time.Duration
		}{{"clean", 0}, {"lat2ms", 2 * time.Millisecond}} {
			for _, flows := range []int{1, 16, 64} {
				name := fmt.Sprintf("pool=%d/%s/flows=%d", pool, link.name, flows)
				b.Run(name, func(b *testing.B) {
					h := newBenchHarness(b, pool, link.latency)
					defer h.close()
					ctx := context.Background()

					conns := make([]net.Conn, flows)
					for i := 0; i < flows; i++ {
						c, err := h.dial(ctx, i+1)
						if err != nil {
							b.Fatalf("dial flow %d: %v", i, err)
						}
						conns[i] = c
						defer c.Close()
					}

					startSink := h.sinkBytes.Load()
					total := int64(b.N) * int64(chunkSize)
					b.SetBytes(int64(chunkSize))
					b.ResetTimer()

					var remaining atomic.Int64
					remaining.Store(int64(b.N))
					var wg sync.WaitGroup
					for i := 0; i < flows; i++ {
						wg.Add(1)
						go func(c net.Conn) {
							defer wg.Done()
							for remaining.Add(-1) >= 0 {
								if _, err := c.Write(chunk); err != nil {
									return
								}
							}
						}(conns[i])
					}
					wg.Wait()

					// Time until the sink has actually received everything (so a
					// latency-shaped link is measured end-to-end, not just the
					// buffer-fill of the writes).
					deadline := time.Now().Add(30 * time.Second)
					for h.sinkBytes.Load()-startSink < total && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					b.StopTimer()

					if got := h.sinkBytes.Load() - startSink; got < total {
						b.Fatalf("sink received %d bytes, sent %d (data lost / stalled)", got, total)
					}
				})
			}
		}
	}
}
