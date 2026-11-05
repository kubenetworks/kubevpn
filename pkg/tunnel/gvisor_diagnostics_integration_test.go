package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/wencaiwulue/kubevpn/v2/pkg/tunnel/internal/netstack"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func nativeTestContext(t *testing.T) (context.Context, *datapathDiagnostics, *diagnosticLog) {
	t.Helper()
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	t.Cleanup(cancel)
	d := newDatapathDiagnostics()
	return withDatapathDiagnostics(ctx, d), d, logs
}

func nativeTestStack(t *testing.T, ctx context.Context, constructor stackConstructor) (*stack.Stack, *channel.Endpoint) {
	t.Helper()
	ep := channel.New(MaxSize, 1500, tcpip.GetRandMacAddr())
	s := constructor(ctx, ep)
	t.Cleanup(func() { s.Destroy(); ep.Close() })
	return s, ep
}

func injectNative(ep *channel.Endpoint, data []byte) {
	p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
	ep.InjectInbound(header.IPv4ProtocolNumber, p)
	p.DecRef()
}

func nativeUDP(payload int) []byte {
	p := make([]byte, 28+payload)
	header.IPv4(p).Encode(&header.IPv4Fields{TotalLength: uint16(len(p)), TTL: 64, Protocol: uint8(udp.ProtocolNumber), SrcAddr: tcpip.AddrFrom4([4]byte{198, 18, 0, 1}), DstAddr: tcpip.AddrFrom4([4]byte{198, 18, 0, 2})})
	header.IPv4(p).SetChecksum(^header.IPv4(p).CalculateChecksum())
	header.UDP(p[20:]).Encode(&header.UDPFields{SrcPort: 1234, DstPort: 4321, Length: uint16(8 + payload)})
	return p
}

func TestGvisorDiagnosticsIntegration_OwnerMalformed(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	d := newDatapathDiagnostics()
	d.interval = 5 * time.Millisecond
	ctx = withDatapathDiagnostics(ctx, d)
	out := make(chan *packet, 8)
	ics := newInterClientStack(ctx, out, tunReserve)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.run(ctx) }()
	defer func() { cancel(); <-ics.done; <-stopped; drainPacketChan(out) }()
	before := ics.stack.Stats().IP.MalformedPacketsReceived.Value()
	// IPv4 version with a truncated header: accepted by InjectIP, rejected by IPv4.
	ics.InjectIP([]byte{0x45, 0, 0, 20})
	after := ics.stack.Stats().IP.MalformedPacketsReceived.Value()
	if after <= before {
		t.Fatalf("malformed packet fixture did not change native counter: before=%d after=%d", before, after)
	}
	t.Logf("native IP.MalformedPacketsReceived changed: %d -> %d", before, after)
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "gvisor_stats") }, "native anomaly log after confirmed malformed packet")
}

func TestGvisorDiagnosticsIntegration_UDPErrors(t *testing.T) {
	for _, constructor := range []stackConstructor{netstack.NewStack, NewLocalStack} {
		ctx, d, logs := nativeTestContext(t)
		s, ep := nativeTestStack(t, ctx, constructor)
		g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
		defer g.close()
		var wq waiter.Queue
		receiver, err := s.NewEndpoint(udp.ProtocolNumber, header.IPv4ProtocolNumber, &wq)
		if err != nil {
			t.Fatal(err)
		}
		defer receiver.Close()
		if err := receiver.Bind(tcpip.FullAddress{NIC: 1, Port: 4321}); err != nil {
			t.Fatal(err)
		}
		receiver.SocketOptions().SetReceiveBufferSize(1024, false)
		badLength := nativeUDP(1)
		binary.BigEndian.PutUint16(badLength[24:26], 100)
		injectNative(ep, badLength)
		badChecksum := nativeUDP(1)
		binary.BigEndian.PutUint16(badChecksum[26:28], 1)
		injectNative(ep, badChecksum)
		for i := 0; i < 8; i++ {
			injectNative(ep, nativeUDP(1000))
		}
		stats := s.Stats().UDP
		if stats.MalformedPacketsReceived.Value() == 0 || stats.ChecksumErrors.Value() == 0 || stats.ReceiveBufferErrors.Value() == 0 {
			t.Fatalf("fixture counters malformed=%d checksum=%d buffer=%d", stats.MalformedPacketsReceived.Value(), stats.ChecksumErrors.Value(), stats.ReceiveBufferErrors.Value())
		}
		d.sample(ctx, time.Now())
		for _, field := range []string{"udp_malformed_packets_received_delta=", "udp_checksum_errors_delta=", "udp_receive_buffer_errors_delta=", "role=client_local scope=stack"} {
			if !strings.Contains(logs.String(), field) {
				t.Fatalf("missing %s: %s", field, logs.String())
			}
		}
		if strings.Contains(logs.String(), "peer_tun_ip=") {
			t.Fatal("shared stack falsely attributed to peer", logs.String())
		}
	}
}

func TestGvisorDiagnosticsIntegration_UDPChecksumOffloadAndHealthy(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make(chan *packet, 1)
	owner := newInterClientStack(ctx, out, datagramHeaderLen)
	defer func() { cancel(); lifetimeAwait(t, owner.done, "offload owner cleanup"); drainPacketChan(out) }()
	plain, plainEP := nativeTestStack(t, ctx, NewLocalStack)
	bind := func(s *stack.Stack) tcpip.Endpoint {
		var wq waiter.Queue
		ep, err := s.NewEndpoint(udp.ProtocolNumber, header.IPv4ProtocolNumber, &wq)
		if err != nil {
			t.Fatal(err)
		}
		if err := ep.Bind(tcpip.FullAddress{NIC: 1, Port: 4321}); err != nil {
			ep.Close()
			t.Fatal(err)
		}
		return ep
	}
	receiver, plainReceiver := bind(owner.stack), bind(plain)
	defer receiver.Close()
	defer plainReceiver.Close()
	valid := nativeUDP(1)
	valid[28] = 42
	owner.InjectIP(valid)
	var received strings.Builder
	if result, err := receiver.Read(&received, tcpip.ReadOptions{}); err != nil || result.Count != 1 || received.String() != string([]byte{42}) {
		t.Fatalf("healthy UDP receive: result=%+v err=%v payload=%q", result, err, received.String())
	}
	d.sample(ctx, time.Now())
	d.sample(ctx, time.Now()) // idle after successful delivery
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatal("healthy/idle UDP warning", logs.String())
	}
	bad := nativeUDP(1)
	binary.BigEndian.PutUint16(bad[26:28], 1)
	before := owner.stack.Stats().UDP.ChecksumErrors.Value()
	owner.InjectIP(bad)
	injectNative(plainEP, bad) // identical bytes without production RX offload
	if owner.stack.Stats().UDP.ChecksumErrors.Value() != before || plain.Stats().UDP.ChecksumErrors.Value() != 1 {
		t.Fatal("unexpected checksum offload accounting")
	}
	received.Reset()
	if result, err := receiver.Read(&received, tcpip.ReadOptions{}); err != nil || result.Count != 1 {
		t.Fatalf("offload packet not delivered: result=%+v err=%v", result, err)
	}
	owner.InjectIP([]byte{0x45, 0, 0, 20}) // make a native report that must exclude checksum deltas
	d.sample(ctx, time.Now())
	if !strings.Contains(logs.String(), "gvisor_stats") || strings.Contains(logs.String(), "udp_checksum_errors_delta=") {
		t.Fatal("offload checksum misreported", logs.String())
	}
}

func TestGvisorDiagnosticsIntegration_StaleSample(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	stale := time.Now().Add(-time.Second)
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	defer g.close()
	start := g.windowStart
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sample(ctx, stale)
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatalf("stale sample emitted a native report: %s", logs.String())
	}
	if !g.windowStart.Equal(start) || g.baseline.counters[3] != 0 {
		t.Fatal("stale sample changed baseline/window")
	}
	d.sample(ctx, start.Add(time.Second))
	if !strings.Contains(logs.String(), "window=1s") || !strings.Contains(logs.String(), "ip_malformed_packets_received_delta=1") {
		t.Fatal("fresh sample lost startup anomaly", logs.String())
	}
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sample(ctx, start.Add(10*time.Second)) // pending, throttled; last observation now 10s
	baseline, pending := g.baseline, g.pending
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sample(ctx, start.Add(5*time.Second)) // newer than report window, older than last observation
	if g.baseline != baseline || g.pending != pending {
		t.Fatal("backward sample mutated pending state")
	}
	d.sample(ctx, start.Add(31*time.Second))
	if strings.Count(logs.String(), "gvisor_stats") != 2 || strings.Contains(logs.String(), "window=-") {
		t.Fatal("bad report window/quiet flush", logs.String())
	}
}

func TestGvisorDiagnosticsIntegration_RefreshNativeObservationTime(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	defer g.close()
	scheduled := g.windowStart.Add(-time.Second)
	observed := g.windowStart.Add(time.Second)
	injectNative(ep, []byte{0x45, 0, 0, 20})
	calls := 0
	d.sampleAt(ctx, scheduled, func() time.Time { calls++; return observed })
	if calls != 1 || !g.lastReport.Equal(observed) || !strings.Contains(logs.String(), "window=1s") {
		t.Fatal("native report used scheduled rather than refreshed time", logs.String())
	}
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sampleAt(ctx, observed, func() time.Time { return observed.Add(29 * time.Second) })
	if strings.Count(logs.String(), "gvisor_stats") != 1 {
		t.Fatal("native throttle used stale report time", logs.String())
	}
	d.sampleAt(ctx, observed.Add(time.Second), func() time.Time { return observed.Add(30 * time.Second) })
	if strings.Count(logs.String(), "gvisor_stats") != 2 {
		t.Fatal("refreshed quiet sample failed to flush", logs.String())
	}
}

func TestGvisorDiagnosticsIntegration_ThrottleBaselineGap(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	injectNative(ep, []byte{0x45, 0, 0, 20}) // historical counter must not be reported
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	defer g.close()
	now := g.windowStart
	d.sample(ctx, now)
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatal("startup reported old counters")
	}
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sample(ctx, now.Add(time.Second))
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sample(ctx, now.Add(2*time.Second))
	if strings.Count(logs.String(), "gvisor_stats") != 1 {
		t.Fatal("throttle failed", logs.String())
	}
	d.sample(ctx, now.Add(40*time.Second)) // no new packets; observation gap must preserve pending
	if strings.Count(logs.String(), "gvisor_stats") != 2 || strings.Count(logs.String(), "ip_malformed_packets_received_delta=1") != 2 {
		t.Fatal("quiet gap discarded pending", logs.String())
	}
}

// Counter replacement is a sampler seam only; real-packet tests above cover delivery.
func TestGvisorDiagnosticsIntegration_CounterReset(t *testing.T) {
	ctx, _, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	defer g.close()
	injectNative(ep, []byte{0x45, 0, 0, 20})
	g.sample(time.Now(), false)
	g.mu.Lock()
	current := g.baseline
	current.counters[3] = 0 // simulate native reset of just this field
	current.counters[0] = 2
	current.established = 0 // gauges never subtract
	g.accumulate(current)
	pending := g.pending
	g.mu.Unlock()
	if pending[3] != 1 || pending[0] != 2 {
		t.Fatal("reset discarded pending or underflowed", pending)
	}
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatal("budget ignored")
	}
}

func TestGvisorDiagnosticsIntegration_TCPTimeout(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, netstack.NewStack)
	addIPv4ToStack(t, s, 1, net.IPv4(198, 18, 0, 2).To4())
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	defer g.close()
	var wq waiter.Queue
	sender, err := s.NewEndpoint(tcp.ProtocolNumber, header.IPv4ProtocolNumber, &wq)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	err = sender.Connect(tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 1}), Port: 4321})
	if _, ok := err.(*tcpip.ErrConnectStarted); !ok {
		t.Fatalf("connect: %v", err)
	}
	// Handshake retransmissions are not counted by this gVisor's aggregate
	// retransmit/RTO counters. Verify real SYN output without claiming otherwise.
	waitFor(t, 3*time.Second, func() bool { return ep.NumQueued() >= 2 }, "real SYN retransmission")
	injectNative(ep, []byte{0x45, 0, 0, 20})
	if ep.NumQueued() < 2 {
		t.Fatal("no retransmitted SYN in output queue")
	}
	d.sample(ctx, time.Now())
	for _, field := range []string{"ip_malformed_packets_received_delta=1", fmt.Sprintf("outbound_queue_depth=%d", ep.NumQueued()), fmt.Sprintf("outbound_queue_capacity=%d", MaxSize)} {
		if !strings.Contains(logs.String(), field) {
			t.Fatal("missing", field, logs.String())
		}
	}
}

func TestGvisorDiagnosticsIntegration_EstablishedRTOAndNormal(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	origin, ep := nativeTestStack(t, ctx, netstack.NewStack)
	remote, remoteEP := nativeTestStack(t, ctx, NewLocalStack)
	addIPv4ToStack(t, origin, 1, net.IPv4(198, 18, 0, 1).To4())
	addIPv4ToStack(t, remote, 1, net.IPv4(198, 18, 0, 2).To4())
	g := diagnosticGvisor(ctx, origin, ep, MaxSize, "client_local", nil)
	defer g.close()
	listener, err := gonet.ListenTCP(remote, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 2}), Port: 4321}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var block atomic.Bool
	var wg sync.WaitGroup
	bridge := func(from, to *channel.Endpoint) {
		defer wg.Done()
		for {
			p := from.ReadContext(ctx)
			if p == nil {
				return
			}
			if !block.Load() {
				view := p.ToView()
				injectNative(to, view.AsSlice())
				view.Release()
			}
			p.DecRef()
		}
	}
	wg.Add(2)
	go bridge(ep, remoteEP)
	go bridge(remoteEP, ep)
	defer func() { cancel(); wg.Wait() }()
	client, err := gonet.DialContextTCP(ctx, origin, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 2}), Port: 4321}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("normal")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(server, make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	d.sample(ctx, time.Now())
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatal("healthy traffic or gauges warned", logs.String())
	}
	if origin.Stats().TCP.CurrentEstablished.Value() == 0 {
		t.Fatal("fixture not established")
	}
	block.Store(true)
	if _, err := client.Write([]byte("withheld")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return origin.Stats().TCP.Retransmits.Value() > 0 && origin.Stats().TCP.Timeouts.Value() > 0
	}, "real established retransmit and RTO")
	d.sample(ctx, time.Now())
	for _, field := range []string{"tcp_retransmits_delta=", "tcp_timeouts_delta=", "tcp_current_established=1"} {
		if !strings.Contains(logs.String(), field) {
			t.Fatal("missing", field, logs.String())
		}
	}
}

func TestGvisorDiagnosticsIntegration_CombinedBudget(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	now := time.Now()
	// More than the detail budget of each kind, all continuously eligible.
	for i := 0; i < 20; i++ {
		stage := diagnosticStage(ctx, fmt.Sprintf("stage%d", i), "test", -1, 0)
		defer stage.close()
		stage.begin()
		stage.start = now.Add(-time.Minute)
		s, ep := nativeTestStack(t, ctx, NewLocalStack)
		g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
		defer g.close()
		injectNative(ep, []byte{0x45, 0, 0, 20})
	}
	for tick := 0; tick < 3; tick++ {
		before := logs.String()
		d.sample(ctx, now.Add(time.Duration(tick)*time.Second))
		text := strings.TrimPrefix(logs.String(), before)
		details := strings.Count(text, "gvisor_stats") + strings.Count(text, "datapath_stall stage=")
		if details > 16 {
			t.Fatalf("combined budget exceeded: %d", details)
		}
	}
	if strings.Count(logs.String(), "gvisor_stats") != 20 || strings.Count(logs.String(), "datapath_stall stage=") != 20 {
		t.Fatal("combined rotation starved sources", logs.String())
	}
}

func TestGvisorDiagnosticsIntegration_ConcurrentCloseJoins(t *testing.T) {
	ctx, d, _ := nativeTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	cancel()
	d.mu.Lock()
	first, second := make(chan struct{}), make(chan struct{})
	go func() { g.sample(time.Now(), true); close(first) }()
	// First close has entered cleanup but cannot remove the registry reference.
	waitFor(t, time.Second, func() bool { return g.closed.Load() }, "first close entered")
	go func() { g.close(); close(second) }()
	premature := false
	select {
	case <-second:
		premature = true
	case <-time.After(25 * time.Millisecond):
	}
	d.mu.Unlock()
	lifetimeAwait(t, first, "first cleanup")
	lifetimeAwait(t, second, "second cleanup")
	if premature {
		t.Fatal("concurrent close returned before registry/reference cleanup completed")
	}
	if len(d.native) != 0 || g.endpoint != nil || g.stats.TCP.Retransmits != nil {
		t.Fatal("close retained references")
	}
}

func TestGvisorDiagnosticsIntegration_Close(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if g := diagnosticGvisor(canceled, s, ep, MaxSize, "client_local", nil); g != nil {
		t.Fatal("registered canceled source")
	}
	child, cancelChild := context.WithCancel(ctx)
	g := diagnosticGvisor(child, s, ep, MaxSize, "client_local", nil)
	injectNative(ep, []byte{0x45, 0, 0, 20})
	cancelChild()
	d.sample(ctx, time.Now())
	g.close()
	g.close()
	if len(d.native) != 0 || g.endpoint != nil || g.stats.TCP.Retransmits != nil {
		t.Fatal("retained native references")
	}
	for _, ref := range d.native[:cap(d.native)] {
		if ref != nil {
			t.Fatal("registry backing reference retained")
		}
	}
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatal("teardown warning", logs.String())
	}
}

func TestGvisorDiagnosticsIntegration_ServerIdentities(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	hub := NewRouter()
	h := GvisorTCPHandler(hub).(*gvisorTCPHandler)
	h.Start(ctx)
	d = h.diagnostics
	t.Cleanup(func() { _ = h.Close() })
	a := net.IPv4(198, 18, 0, 10).To4()
	b := net.IPv4(198, 18, 0, 11).To4()
	csA := h.getOrCreateClientStack(string(a))
	csB := h.getOrCreateClientStack(string(b))
	defer func() {
		csA.cancel()
		csB.cancel()
		lifetimeAwait(t, csA.done, "A cleanup")
		lifetimeAwait(t, csB.done, "B cleanup")
	}()
	source := func(ep *channel.Endpoint) *gvisorDiagnostic {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, g := range d.native {
			if g.endpoint == ep {
				return g
			}
		}
		t.Fatal("owner native source absent")
		return nil
	}
	ga, gb := source(csA.endpoint), source(csB.endpoint)
	if ga.id == gb.id {
		t.Fatal("stack IDs collide")
	}
	// Real route departure/re-add retains the stack and its source baseline.
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	hub.AddRoute(ctx, a, x)
	hub.RemoveRoutesByConn(ctx, x)
	if retained := h.getOrCreateClientStack(string(a)); retained != csA {
		t.Fatal("reconnect replaced retained stack")
	}
	hub.AddRoute(ctx, a, x)
	if source(csA.endpoint) != ga {
		t.Fatal("reconnect reset diagnostic identity")
	}
	injectNative(csA.endpoint, []byte{0x45, 0, 0, 20})
	injectNative(csB.endpoint, []byte{0x45, 0, 0, 20})
	injectNative(csB.endpoint, []byte{0x45, 0, 0, 20})
	// Start owns the sampler; do not race its tick with a manual sample.
	waitFor(t, 3*time.Second, func() bool { return strings.Count(logs.String(), "gvisor_stats") == 2 }, "service native reports")
	for _, g := range []*gvisorDiagnostic{ga, gb} {
		if !strings.Contains(logs.String(), fmt.Sprintf("stack_id=%d role=server_client scope=stack", g.id)) {
			t.Fatal("missing server source", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "peer_tun_ip=198.18.0.10") || !strings.Contains(logs.String(), "peer_tun_ip=198.18.0.11") || !strings.Contains(logs.String(), "ip_malformed_packets_received_delta=2") {
		t.Fatal(logs.String())
	}
	csA.cancel()
	lifetimeAwait(t, csA.done, "retired stack")
	replacement := h.getOrCreateClientStack(string(a))
	defer func() { replacement.cancel(); lifetimeAwait(t, replacement.done, "replacement cleanup") }()
	if source(replacement.endpoint).id == ga.id {
		t.Fatal("replacement reused ID")
	}
	if !ga.closed.Load() || ga.endpoint != nil {
		t.Fatal("retirement retained source")
	}
}

func TestGvisorDiagnosticsIntegration_PublicationRaces(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, d, _ := nativeTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			h := GvisorTCPHandler(NewRouter()).(*gvisorTCPHandler)
			h.Start(ctx)
			d = h.diagnostics
			t.Cleanup(func() { _ = h.Close() })
			entered, release := make(chan struct{}, 2), make(chan struct{})
			h.newStack = func(ctx context.Context, ep stack.LinkEndpoint) *stack.Stack {
				s := NewLocalStack(ctx, ep)
				entered <- struct{}{}
				<-release
				return s
			}
			results := make(chan *clientStack, 2)
			key := string(net.IPv4(198, 18, 0, 19).To4())
			go func() { results <- h.getOrCreateClientStack(key) }()
			go func() { results <- h.getOrCreateClientStack(key) }()
			<-entered
			<-entered
			if canceled {
				cancel()
			}
			close(release)
			a, b := <-results, <-results
			if canceled {
				if a != nil || b != nil || len(d.native) != 0 {
					t.Fatal("canceled construction retained source")
				}
				return
			}
			if a != b || a == nil {
				t.Fatal("losing construction published")
			}
			defer func() { a.cancel(); lifetimeAwait(t, a.done, "published race winner") }()
			if len(d.native) != 1 {
				t.Fatalf("registered losing construction: %d", len(d.native))
			}
		})
	}
}

func TestGvisorDiagnosticsIntegration_LocalOwner(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	in, out := make(chan *packet, 1), make(chan *packet, 1)
	h := handleGvisorPacket(in, out, datagramHeaderLen)
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	var p *packet
	defer func() {
		cancel()
		lifetimeAwait(t, done, "local owner")
		drainPacketChan(in)
		drainPacketChan(out)
		if p != nil {
			checkBatchRefs(t, []*packet{p})
		}
	}()
	waitFor(t, time.Second, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return len(d.native) == 1 }, "local owner registration")
	// Deliver via the actual local-owner input channel, not directly to its endpoint.
	p = batchPacket(tunReserve+4, 0)
	copy(p.data[tunReserve:], []byte{0x45, 0, 0, 20})
	p.length = typePrefixLen + 4
	in <- p
	waitFor(t, time.Second, func() bool {
		d.sample(ctx, time.Now())
		return strings.Contains(logs.String(), "role=client_local scope=stack")
	}, "local channel native anomaly")
	cancel()
	lifetimeAwait(t, done, "local teardown")
	if len(d.native) != 0 {
		t.Fatal("local native source leaked")
	}
}

func TestGvisorDiagnosticsIntegration_RSTOnlyAndContext(t *testing.T) {
	ctx, d, logs := nativeTestContext(t)
	s, ep := nativeTestStack(t, ctx, NewLocalStack)
	ep.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
	defer g.close()
	p := make([]byte, 40)
	header.IPv4(p).Encode(&header.IPv4Fields{TotalLength: 40, TTL: 64, Protocol: uint8(tcp.ProtocolNumber), SrcAddr: tcpip.AddrFrom4([4]byte{198, 18, 0, 1}), DstAddr: tcpip.AddrFrom4([4]byte{198, 18, 0, 2})})
	header.IPv4(p).SetChecksum(^header.IPv4(p).CalculateChecksum())
	header.TCP(p[20:]).Encode(&header.TCPFields{SrcPort: 1234, DstPort: 4321, DataOffset: 20, Flags: header.TCPFlagAck, AckNum: 1})
	injectNative(ep, p)
	waitFor(t, time.Second, func() bool { return s.Stats().TCP.ResetsSent.Value() > 0 }, "native RST response")
	d.sample(ctx, time.Now())
	if strings.Contains(logs.String(), "gvisor_stats") {
		t.Fatal("RST-only warning", logs.String())
	}
	injectNative(ep, p)
	waitFor(t, time.Second, func() bool { return s.Stats().TCP.ResetsSent.Value() > 1 }, "second native RST response")
	injectNative(ep, []byte{0x45, 0, 0, 20})
	d.sample(ctx, time.Now())
	if !strings.Contains(logs.String(), "tcp_resets_sent_delta=1") || !strings.Contains(logs.String(), "outbound_queue_depth=2") {
		t.Fatal("RST context or actual response queue missing", logs.String())
	}
}

type nativeBlockingLog struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *nativeBlockingLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "gvisor_stats") {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	return len(p), nil
}

func TestGvisorDiagnosticsIntegration_DestroyDoesNotWaitForLog(t *testing.T) {
	writer := &nativeBlockingLog{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), writer)))
	defer cancel()
	d := newDatapathDiagnostics()
	ctx = withDatapathDiagnostics(ctx, d)
	out := make(chan *packet, 1)
	owner := newInterClientStack(ctx, out, datagramHeaderLen)
	owner.InjectIP([]byte{0x45, 0, 0, 20})
	done := make(chan struct{})
	go func() { defer close(done); d.sample(ctx, time.Now()) }()
	defer func() {
		close(writer.release)
		lifetimeAwait(t, done, "sampler log release")
		cancel()
		lifetimeAwait(t, owner.done, "stack cleanup")
		drainPacketChan(out)
	}()
	lifetimeAwait(t, writer.entered, "native logger entry")
	cancel()
	lifetimeAwait(t, owner.done, "stack destruction while logger blocked")
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.native) != 0 {
		t.Fatal("blocked logger retained source registry")
	}
}

func BenchmarkGvisorDiagnosticsSample(b *testing.B) {
	for _, count := range []int{1, 16, 64} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), io.Discard))
			d := newDatapathDiagnostics()
			ctx = withDatapathDiagnostics(ctx, d)
			for i := 0; i < count; i++ {
				ep := channel.New(MaxSize, 1500, tcpip.GetRandMacAddr())
				s := NewLocalStack(ctx, ep)
				g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
				defer func() { g.close(); s.Destroy(); ep.Close() }()
			}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d.sample(ctx, now.Add(time.Duration(i)*time.Second))
			}
		})
	}
}

// One benchmark operation is a real owner tick, not a packet-throughput proxy.
func BenchmarkGvisorDiagnosticsTick(b *testing.B) {
	ctx := plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), io.Discard))
	d := newDatapathDiagnostics()
	ctx = withDatapathDiagnostics(ctx, d)
	for i := 0; i < 16; i++ {
		ep := channel.New(MaxSize, 1500, tcpip.GetRandMacAddr())
		s := NewLocalStack(ctx, ep)
		g := diagnosticGvisor(ctx, s, ep, MaxSize, "client_local", nil)
		defer func() { g.close(); s.Destroy(); ep.Close() }()
	}
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.sample(ctx, <-ticker.C)
	}
}
