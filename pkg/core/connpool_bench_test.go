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
// expect pool size to matter only with MANY concurrent flows. On a clean loopback a
// single conn is rarely the bottleneck (expect ~no gain); the pool's value shows on a
// latency-shaped link where one conn is round-trip-limited and N conns parallelize.
//
// Run:
//	go test ./pkg/core/ -run '^$' -bench BenchmarkConnPool -benchmem -benchtime=2s -count=3

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

func (c *packetConn) Close() error                       { c.closeO.Do(func() { close(c.done) }); return nil }
func (c *packetConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c *packetConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c *packetConn) SetDeadline(time.Time) error        { return nil }
func (c *packetConn) SetReadDeadline(time.Time) error    { return nil }
func (c *packetConn) SetWriteDeadline(time.Time) error   { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "packetconn" }
func (dummyAddr) String() string  { return "packetconn" }

// ---------------------------------------------------------------------------
// shapedConn: wraps a net.Conn and adds a fixed per-read latency, modelling a
// link RTT. A single conn serializes flows through one delayed stream (round-trip
// limited); N conns overlap their delays. (We deliberately do NOT cap per-conn
// bandwidth, which would unfairly multiply the ceiling by the conn count.)
// ---------------------------------------------------------------------------

type shapedConn struct {
	net.Conn
	latency time.Duration
}

func (c *shapedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.latency > 0 {
		time.Sleep(c.latency)
	}
	return n, err
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
				c = &shapedConn{Conn: c, latency: latency}
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
