package tunnel

import (
	"bytes"
	"context"
	"github.com/sirupsen/logrus"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type diagnosticLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *diagnosticLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *diagnosticLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestDatapathDiagnosticsIntegration_DefaultTUNDrops(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	dev := &tunDevice{tun: a, tunInbound: make(chan *packet, 1), tunOutbound: make(chan *packet, 1), errChan: errChanOf()}
	dev.transport = newServerTransport(dev, NewRouter())
	done := make(chan struct{})
	go func() { defer close(done); serve(ctx, dev) }()
	defer func() { cancel(); a.Close(); <-done }()
	if _, err := b.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(logs.String(), "reason=invalid") }, "default device drop sampler")
}

func TestDatapathDiagnosticsIntegration_DefaultHandler(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	h := GvisorTCPHandler(NewRouter()).(*gvisorTCPHandler)
	h.Start(ctx)
	t.Cleanup(func() { _ = h.Close() })
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	connCtx, stopConn := context.WithCancel(ctx)
	defer stopConn()
	done := make(chan struct{})
	go func() { defer close(done); h.Handle(connCtx, a) }()
	ip := net.IPv4(198, 18, 0, 77).To4()
	if _, err := b.Write(frameDatagram(packetTypeToGvisor, genEchoRequest(t, ip.String(), config.RouterIP.String()))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, func() bool { return strings.Contains(logs.String(), "datapath_stall stage=socket_write") }, "default handler sampler reports blocked echo")
	stopConn()
	b.Close()
	<-done
	select {
	case <-h.diagnosticDone:
		t.Fatal("accepted connection stopped the service sampler")
	default:
	}
	cancel()
	_ = h.Close()
	select {
	case <-h.diagnosticDone:
	default:
		t.Fatal("sampler not joined")
	}
	if strings.Contains(logs.String(), "datapath_recovered") {
		t.Fatal("shutdown claimed recovery")
	}
}

func TestDatapathDiagnosticsIntegration_SlotInFlight(t *testing.T) {
	for _, cancelBlocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery", true: "cancellation"}[cancelBlocked], func(t *testing.T) {
			logs := &diagnosticLog{}
			ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
			defer cancel()
			d := newDatapathDiagnostics()
			d.interval = 5 * time.Millisecond
			d.threshold = 25 * time.Millisecond
			ctx = withDatapathDiagnostics(ctx, d)
			stopped := make(chan struct{})
			go func() { defer close(stopped); d.run(ctx) }()
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			q := make(chan *packet, 1)
			p := batchPacket(32, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				(&connAttempt{connSlot: &connSlot{id: 0, inbound: q}}).writeGeneration(ctx, a, errChanOf(), 0)
			}()
			// An idle writer is not a stalled writer.
			time.Sleep(50 * time.Millisecond)
			if strings.Contains(logs.String(), "datapath_stall") {
				t.Fatal("idle warning")
			}
			q <- p
			waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_stall") }, "warning before socket release")
			if len(q) != 0 {
				t.Fatal("test did not dequeue batch")
			}
			if !strings.Contains(logs.String(), "stage=socket_write") {
				t.Fatal(logs.String())
			}
			if cancelBlocked {
				cancel()
				a.Close()
			} else {
				if _, err := io.ReadFull(b, make([]byte, 32)); err != nil {
					t.Fatal(err)
				}
				waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_recovered") }, "recovery")
				cancel()
			}
			<-done
			<-stopped
			want := 1
			if cancelBlocked {
				want = 0
			}
			if n := strings.Count(logs.String(), "datapath_recovered"); n != want {
				t.Fatalf("recovered=%d want=%d: %s", n, want, logs.String())
			}
			checkBatchRefs(t, []*packet{p})
		})
	}
}

func TestDatapathDiagnosticsIntegration_TUNAndBuffered(t *testing.T) {
	for _, kind := range []string{"tun_write", "socket_write"} {
		t.Run(kind, func(t *testing.T) {
			logs := &diagnosticLog{}
			ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
			defer cancel()
			d := newDatapathDiagnostics()
			d.interval = 5 * time.Millisecond
			d.threshold = 25 * time.Millisecond
			ctx = withDatapathDiagnostics(ctx, d)
			stopped := make(chan struct{})
			go func() { defer close(stopped); d.run(ctx) }()
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			p := batchPacket(32, 1)
			done := make(chan struct{})
			if kind == "tun_write" {
				q := make(chan *packet, 1)
				q <- p
				dev := &tunDevice{tun: a, tunOutbound: q, errChan: errChanOf()}
				go func() { defer close(done); dev.writeToTun(ctx) }()
			} else {
				c := newBufferedTCP(ctx, a, nil)
				if err := c.writePacket(ctx, p); err != nil {
					t.Fatal(err)
				}
				go func() { <-ctx.Done(); c.Close(); close(done) }()
			}
			waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_stall stage="+kind) }, "blocked "+kind)
			n := 32
			if kind == "tun_write" {
				n -= tunReserve
			}
			if _, err := io.ReadFull(b, make([]byte, n)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_recovered") }, "recovery "+kind)
			cancel()
			<-done
			<-stopped
			waitFor(t, time.Second, func() bool { return p.refs.Load() == 0 }, "packet release")
			checkBatchRefs(t, []*packet{p})
		})
	}
}

func TestDatapathDiagnosticsIntegration_BlockedProducers(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	d := newDatapathDiagnostics()
	d.interval = 5 * time.Millisecond
	d.threshold = 25 * time.Millisecond
	ctx = withDatapathDiagnostics(ctx, d)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.run(ctx) }()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := newBufferedTCP(ctx, a, nil)
	var packets []*packet
	p0 := batchPacket(32, 0)
	packets = append(packets, p0)
	if err := c.writePacket(ctx, p0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "stage=socket_write") }, "socket blocked before filling queue")
	for i := 0; i < MaxSize; i++ {
		p := batchPacket(32, i)
		packets = append(packets, p)
		if err := c.writePacket(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, time.Second, func() bool { return len(c.ch) == MaxSize }, "buffer full")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		p := batchPacket(32, i)
		packets = append(packets, p)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.writePacket(ctx, p) != nil {
				p.release()
			}
		}()
	}
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "stage=buffered_enqueue") }, "independent enqueue wait evidence")
	hub := NewRouter()
	hub.AddRoute(ctx, net.IPv4(198, 18, 0, 5), c)
	metadataDone := make(chan struct{})
	go func() { hub.RemoveRoutesByConn(ctx, c); close(metadataDone) }()
	select {
	case <-metadataDone:
	case <-time.After(time.Second):
		t.Fatal("queue blocked metadata")
	}
	cancel()
	c.Close()
	wg.Wait()
	<-stopped
	waitFor(t, time.Second, func() bool {
		for _, p := range packets {
			if p.refs.Load() != 0 {
				return false
			}
		}
		return true
	}, "all buffered refs released")
	if strings.Contains(logs.String(), "datapath_recovered") {
		t.Fatal("cancellation claimed recovery")
	}
	checkBatchRefs(t, packets)
}

func TestDatapathDiagnosticsIntegration_RawRouteIsolation(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	d := newDatapathDiagnostics()
	d.interval = 5 * time.Millisecond
	d.threshold = 25 * time.Millisecond
	ctx = withDatapathDiagnostics(ctx, d)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.run(ctx) }()
	hub := NewRouter()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ip := net.IPv4(198, 18, 0, 5)
	hub.AddRoute(ctx, ip, a)
	done := make(chan struct{})
	p := batchPacket(32, 1)
	go func() { defer close(done); hub.WriteToRoutePacket(ctx, string(ip), p); p.release() }()
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "stage=raw_socket_write") }, "raw route active write")
	gateDone := make(chan struct{})
	q := batchPacket(32, 2)
	go func() { defer close(gateDone); hub.WriteToRoutePacket(ctx, string(ip), q); q.release() }()
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "stage=raw_write_gate") }, "raw route gate wait")
	c, e := net.Pipe()
	defer c.Close()
	defer e.Close()
	other := net.IPv4(198, 18, 0, 6)
	hub.AddRoute(ctx, other, c)
	healthy := make(chan error, 1)
	go func() { _, err := io.ReadFull(e, make([]byte, 32)); healthy <- err }()
	r := batchPacket(32, 3)
	if _, err := hub.WriteToRoutePacket(ctx, string(other), r); err != nil {
		t.Fatal(err)
	}
	r.release()
	if err := <-healthy; err != nil {
		t.Fatal(err)
	}
	hub.RemoveRoutesByConn(ctx, c)
	cancel()
	<-done
	<-gateDone
	<-stopped
	if strings.Contains(logs.String(), "datapath_recovered") {
		t.Fatal("cancellation claimed recovery")
	}
	checkBatchRefs(t, []*packet{p, q, r})
}

func TestDatapathDiagnosticsIntegration_TargetDial(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithTimeout(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)), 5*time.Second)
	defer cancel()
	ep := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	ep.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	origin := NewLocalStack(ctx, ep)
	defer origin.Destroy()
	ip := net.IPv4(198, 18, 0, 63).To4()
	addIPv4ToStack(t, origin, 1, ip)
	remoteEP := channel.New(MaxSize, uint32(config.DefaultMTU), tcpip.GetRandMacAddr())
	remoteEP.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	remote := NewLocalStack(ctx, remoteEP)
	defer remote.Destroy()
	var wg sync.WaitGroup
	bridge := func(from, to *channel.Endpoint) {
		defer wg.Done()
		for ctx.Err() == nil {
			p := from.ReadContext(ctx)
			if p == nil {
				return
			}
			view := p.ToView()
			copy := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(view.AsSlice())})
			to.InjectInbound(p.NetworkProtocolNumber, copy)
			copy.DecRef()
			view.Release()
			p.DecRef()
		}
	}
	wg.Add(2)
	go bridge(ep, remoteEP)
	go bridge(remoteEP, ep)
	defer func() { cancel(); wg.Wait() }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dial := func(port int) net.Conn {
		t.Helper()
		c, err := gonet.DialContextTCP(ctx, origin, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 96, 0, 10}), Port: uint16(port)}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := dial(port)
	defer c.Close()
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "target_dial") }, "local target refusal on original info logger")
	if !strings.Contains(logs.String(), "role=local") || !strings.Contains(logs.String(), "reason=refused") || !strings.Contains(logs.String(), "source_tun_ip=198.18.0.63") {
		t.Fatal(logs.String())
	}
	healthy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, err := healthy.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		io.Copy(c, c)
	}()
	app := dial(healthy.Addr().(*net.TCPAddr).Port)
	app.SetDeadline(time.Now().Add(time.Second))
	if _, err := app.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(app, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	app.Close()
	<-echoDone
	if strings.Count(logs.String(), "target_dial") != 1 {
		t.Fatal("healthy dial changed failure summary", logs.String())
	}
}

type diagnosticPacketContext struct {
	context.Context
	packet *packet
	reused bool
}

func (c *diagnosticPacketContext) Err() error {
	if c.packet.refs.Load() < 0 {
		// Simulate immediate reuse of the released buffer before report reads its IP.
		copy(c.packet.dst, net.IPv4(198, 18, 0, 99).To4())
		c.reused = true
	}
	return c.Context.Err()
}

func TestDatapathDiagnosticsIntegration_DropRetainsPacket(t *testing.T) {
	logs := &diagnosticLog{}
	ctx := plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs))
	p := batchPacket(32, 1)
	p.release() // Transfer the only remaining owner to the route queue.
	p.dst = net.IP(p.data[tunReserve : tunReserve+4])
	copy(p.dst, net.IPv4(198, 18, 0, 5).To4())
	observed := &diagnosticPacketContext{Context: ctx, packet: p}
	dev := &tunDevice{tunInbound: make(chan *packet, 1)}
	dev.tunInbound <- p
	close(dev.tunInbound)
	transport := newServerTransport(dev, NewRouter())
	done := make(chan struct{})
	go func() { defer close(done); transport.routeTun(observed) }()
	lifetimeAwait(t, done, "route-missing drop")
	if refs := p.refs.Load(); refs != -1 {
		t.Fatalf("packet not released after reporting: refs=%d", refs)
	}
	dev.drops.flush(ctx, time.Now())
	if observed.reused || !strings.Contains(logs.String(), "peer_tun_ip=198.18.0.5") {
		t.Fatalf("drop read peer after packet release: %s", logs.String())
	}
}

func TestDatapathDiagnosticsIntegration_DispatchWarningOutcome(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "successful"}[success], func(t *testing.T) {
			logs := &diagnosticLog{}
			ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
			defer cancel()
			d := newDatapathDiagnostics()
			ctx = withDatapathDiagnostics(ctx, d)
			hub := NewRouter()
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			c, e := net.Pipe()
			defer c.Close()
			defer e.Close()
			first, second := net.IPv4(198, 18, 0, 5), net.IPv4(198, 18, 0, 6)
			hub.AddRoute(ctx, first, a)
			hub.AddRoute(ctx, second, c)
			dev := &tunDevice{tunInbound: make(chan *packet, 1)}
			transport := newServerTransport(dev, hub)
			done := make(chan struct{})
			go func() { defer close(done); transport.routeTun(ctx) }()
			defer func() { cancel(); lifetimeAwait(t, done, "dispatch cleanup") }()
			p, q := batchPacket(32, 1), batchPacket(32, 2)
			p.dst, q.dst = first, second
			dev.tunInbound <- p
			var stage *datapathStage
			waitFor(t, time.Second, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				for _, s := range d.stages {
					if s.name == "route_dispatch" {
						s.mu.Lock()
						active := s.active
						s.mu.Unlock()
						if active {
							stage = s
							return true
						}
					}
				}
				return false
			}, "blocked real route dispatch")
			stage.mu.Lock()
			now := stage.start.Add(6 * time.Second)
			stage.mu.Unlock()
			d.sample(ctx, now)
			if !strings.Contains(logs.String(), "datapath_stall stage=route_dispatch") {
				t.Fatal(logs.String())
			}
			if success {
				if _, err := io.ReadFull(b, make([]byte, 32)); err != nil {
					t.Fatal(err)
				}
			} else {
				b.Close()
			}
			// A different destination succeeds before the next owner tick.
			dev.tunInbound <- q
			if _, err := io.ReadFull(e, make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, time.Second, func() bool {
				stage.mu.Lock()
				defer stage.mu.Unlock()
				return stage.token == 2 && !stage.active
			}, "both dispatch outcomes recorded")
			d.sample(ctx, now.Add(time.Millisecond))
			d.sample(ctx, now.Add(2*time.Millisecond))
			want := 0
			if success {
				want = 1
			}
			if got := strings.Count(logs.String(), "datapath_recovered stage=route_dispatch"); got != want {
				t.Fatalf("warned success=%v recovered=%d want=%d: %s", success, got, want, logs.String())
			}
			stage.mu.Lock()
			completed, packets, bytes := stage.completed, stage.packets, stage.bytes
			stage.mu.Unlock()
			if completed != uint64(1+want) || packets != completed || bytes != completed*30 {
				t.Fatalf("inaccurate progress: completed=%d packets=%d bytes=%d", completed, packets, bytes)
			}
			cancel()
			lifetimeAwait(t, done, "dispatch completion")
			checkBatchRefs(t, []*packet{p, q})
		})
	}
}

func TestDatapathDiagnosticsIntegration_DropReasons(t *testing.T) {
	logs := &diagnosticLog{}
	ctx := plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs))
	hub := NewRouter()
	h := GvisorTCPHandler(hub).(*gvisorTCPHandler)
	p := batchPacket(32, 1)
	defer checkBatchRefs(t, []*packet{p})
	defer p.release()
	ip := net.IPv4(198, 18, 0, 5)
	for i := 0; i < 3; i++ {
		_, err := hub.WriteToRoutePacket(ctx, string(ip), p)
		h.reportRouteDrop(ctx, err, ip)
	}
	if logs.String() != "" {
		t.Fatal("packet drop performed synchronous log I/O", logs.String())
	}
	if h.drops.counts[dropRouteMissing] != 3 {
		t.Fatal("suppressed drops not counted")
	}
	now := time.Now()
	h.drops.flush(ctx, now)
	if strings.Count(logs.String(), "reason=route_missing") != 1 || !strings.Contains(logs.String(), "dropped_delta=3") {
		t.Fatal(logs.String())
	}
	a, b := net.Pipe()
	a.Close()
	defer b.Close()
	hub.AddRoute(ctx, ip, a)
	_, err := hub.WriteToRoutePacket(ctx, string(ip), p)
	h.reportRouteDrop(ctx, err, ip)
	h.drops.flush(ctx, now)
	if h.drops.counts[dropRoutesFailed] != 1 || !strings.Contains(logs.String(), "reason=all_routes_failed") {
		t.Fatal(logs.String())
	}
	if strings.Contains(logs.String(), "not announced") {
		t.Fatal("misleading route explanation")
	}
	other := net.IPv4(198, 18, 0, 6)
	_, err = hub.WriteToRoutePacket(ctx, string(other), p)
	h.reportRouteDrop(ctx, err, other)
	_, err = hub.WriteToRoutePacket(ctx, string(ip), p)
	h.reportRouteDrop(ctx, err, ip)
	h.drops.flush(ctx, now.Add(time.Second))
	if !strings.Contains(logs.String(), "peer_tun_ip=198.18.0.6") {
		t.Fatal("another peer's throttle hid this peer", logs.String())
	}
	before := strings.Count(logs.String(), "reason=route_missing")
	h.drops.flush(ctx, now.Add(31*time.Second))
	if strings.Count(logs.String(), "reason=route_missing") != before+1 {
		t.Fatal("suppressed drop lost after traffic stopped", logs.String())
	}
	h.drops.flush(ctx, now.Add(62*time.Second))
	if len(h.drops.peers) != 0 {
		t.Fatal("quiet drop identities retained")
	}
}

func TestDatapathDiagnosticsIntegration_WaiterOutcome(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "success"}[success], func(t *testing.T) {
			logs := &diagnosticLog{}
			ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
			defer cancel()
			d := newDatapathDiagnostics()
			d.interval = 5 * time.Millisecond
			d.threshold = 25 * time.Millisecond
			ctx = withDatapathDiagnostics(ctx, d)
			stopped := make(chan struct{})
			go func() { defer close(stopped); d.run(ctx) }()
			defer func() { cancel(); <-stopped }()
			hub := NewRouter()
			hub.bridge.queue = make(chan *packet, 1)
			stage := diagnosticStage(ctx, "tun_out_enqueue", "server", -1, 0)
			defer stage.close()
			// A distinct producer succeeds while the oldest, warned producer remains blocked.
			otherQueue := make(chan *packet)
			other := batchPacket(32, 1)
			otherDone := make(chan struct{})
			go func() { diagnosticEnqueue(ctx, otherQueue, other, stage); close(otherDone) }()
			p := batchPacket(32, 2)
			hub.bridge.queue <- p
			producerCtx, stopProducer := context.WithCancel(ctx)
			defer stopProducer()
			waiting := batchPacket(32, 3)
			done := make(chan struct{})
			go func() {
				if !hub.bridge.sendObserved(producerCtx, waiting, stage) {
					waiting.release()
				}
				close(done)
			}()
			waitFor(t, time.Second, func() bool {
				stage.mu.Lock()
				defer stage.mu.Unlock()
				return stage.waits != nil && stage.waits.next != nil
			}, "two waiting producers")
			waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_stall") }, "waiters warned")
			(<-otherQueue).release()
			<-otherDone
			if success {
				(<-hub.bridge.queue).release()
				<-done
				(<-hub.bridge.queue).release()
			} else {
				stopProducer()
				<-done
				(<-hub.bridge.queue).release()
			}
			time.Sleep(50 * time.Millisecond)
			count := strings.Count(logs.String(), "datapath_recovered")
			if success && count != 1 || !success && count != 0 {
				t.Fatalf("success=%v recovered=%d: %s", success, count, logs.String())
			}
			if ctx.Err() != nil {
				t.Fatal("owner canceled during test")
			}
			checkBatchRefs(t, []*packet{p, other, waiting})
		})
	}
}

func TestDatapathDiagnosticsIntegration_BridgeQueueAndRelease(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	d := newDatapathDiagnostics()
	d.interval = 5 * time.Millisecond
	d.threshold = 25 * time.Millisecond
	ctx = withDatapathDiagnostics(ctx, d)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.run(ctx) }()
	defer func() { cancel(); <-stopped }()
	hub := NewRouter()
	hub.bridge.queue = make(chan *packet, 1)
	stage := diagnosticStage(ctx, "tun_out_enqueue", "server", -1, 0)
	p, q := batchPacket(32, 1), batchPacket(32, 2)
	hub.bridge.queue <- p
	producerCtx, stopProducer := context.WithCancel(ctx)
	defer stopProducer()
	done := make(chan struct{})
	go func() {
		if !hub.bridge.sendObserved(producerCtx, q, stage) {
			q.release()
		}
		close(done)
	}()
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_stall") }, "full bridge reported")
	if !strings.Contains(logs.String(), "queue_depth=1 queue_capacity=1") {
		t.Error(logs.String())
	}
	stopProducer()
	<-done
	hub.bridge.stop()
	stage.close()
	d.mu.Lock()
	backing := d.stages[:cap(d.stages)]
	for i, p := range backing {
		if p != nil {
			t.Errorf("removed stage retained at backing index %d", i)
		}
	}
	d.mu.Unlock()
	checkBatchRefs(t, []*packet{p, q})
}

func TestDatapathDiagnosticsIntegration_DispatchSessionScope(t *testing.T) {
	logs := &diagnosticLog{}
	ctx, cancel := context.WithCancel(plog.WithLogger(context.Background(), plog.GetLoggerForClient(int32(logrus.InfoLevel), logs)))
	defer cancel()
	d := newDatapathDiagnostics()
	d.interval = 5 * time.Millisecond
	d.threshold = 25 * time.Millisecond
	ctx = withDatapathDiagnostics(ctx, d)
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.run(ctx) }()
	stats := &HeartbeatStats{}
	dev := &tunDevice{tunInbound: make(chan *packet, MaxSize+2), tunOutbound: make(chan *packet, 1)}
	ct := newClientTransport(dev, &DialConfig{DialContext: func(ctx context.Context) (*framing.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}, stats)
	done := make(chan struct{})
	go func() { defer close(done); ct.runConnPool(ctx) }()
	defer func() { cancel(); <-done; <-stopped; ct.close() }()
	waitFor(t, time.Second, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, s := range d.stages {
			if s.name == "slot_enqueue" {
				return true
			}
		}
		return false
	}, "dispatcher registered")
	stats.BeginGeneration()
	var packets []*packet
	for i := 0; i < MaxSize+1; i++ {
		p := batchPacket(32, i)
		p.dst = net.IPv4(10, 0, 0, 1)
		packets = append(packets, p)
		dev.tunInbound <- p
	}
	waitFor(t, time.Second, func() bool { return strings.Contains(logs.String(), "datapath_stall stage=slot_enqueue") }, "dispatcher blocked after generation change")
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "datapath_stall stage=slot_enqueue") && (!strings.Contains(line, "scope=session") || strings.Contains(line, "generation=")) {
			t.Error("dispatcher falsely attributed to transport generation: ", line)
		}
	}
	cancel()
	<-done
	ct.close()
	checkBatchRefs(t, packets)
}

func TestHeartbeatIntegration_QueuedOwnership(t *testing.T) {
	for _, mode := range []string{"retired_writer", "deadline_failure", "invalid_probe"} {
		t.Run(mode, func(t *testing.T) {
			stats := &HeartbeatStats{}
			q := make(chan *packet, 1)
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			generation := stats.Generation()
			if mode == "retired_writer" {
				stats.BeginGeneration()
			}
			ip := genEchoRequest(t, regTestTunIPv4, config.RouterIP.String())
			p := batchPacket(tunReserve+len(ip), 0)
			copy(p.data[tunReserve:], ip)
			p.data[datagramHeaderLen] = packetTypeControl
			if mode == "invalid_probe" {
				p.data[tunReserve] = 0
			}
			stats.enqueueProbe(q, p)
			close(q)
			if mode == "deadline_failure" {
				a.Close()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				(&connAttempt{connSlot: &connSlot{id: ConnPoolSize, role: slotRoleControl, stats: stats, inbound: q}}).writeGeneration(context.Background(), a, errChanOf(), generation)
			}()
			if mode == "invalid_probe" {
				peer, _ := framing.NewConn(context.Background(), b)
				b.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := peer.Read(make([]byte, 256)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("writer did not finish")
			}
			if got := stats.Snapshot().ProbeState; got != "not_queued" {
				t.Fatalf("phantom admission after %s: %s", mode, got)
			}
			checkBatchRefs(t, []*packet{p})
		})
	}
}

func TestHeartbeatIntegration_ProbeState(t *testing.T) {
	stats := &HeartbeatStats{}
	if got := stats.Snapshot().ProbeState; got != "not_queued" {
		t.Fatalf("initial state %q", got)
	}
	wire := newProbeWire(t, stats)
	request := wire.send(false)
	if got := stats.Snapshot().ProbeState; got != "written_no_reply" {
		t.Fatalf("written state %q", got)
	}
	wire.deliver(wire.echo(request))
	if got := stats.Snapshot().ProbeState; got != "matched_reply" {
		t.Fatalf("reply state %q", got)
	}
	wire.send(false)
	if got := stats.Snapshot().ProbeState; got != "written_no_reply" {
		t.Fatalf("new unanswered probe hidden by old reply %q", got)
	}
	stats.ClearPending()
	if got := stats.Snapshot().ProbeState; got != "not_queued" {
		t.Fatalf("clear retained cumulative evidence %q", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ct := &clientTransport{stats: stats, dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) { return net.ParseIP(regTestTunIPv4), nil, nil }}, controlSlot: &connSlot{inbound: make(chan *packet, 8)}}
	done := make(chan struct{})
	go func() { defer close(done); ct.heartbeats(ctx) }()
	waitFor(t, time.Second, func() bool { return stats.Snapshot().ProbeState == "queued_not_written" }, "queued evidence")
	cancel()
	<-done
	stats.ClearPending()
	// Clear resets diagnostic evidence, not the transmission of queued probes.
	// Correlation is stamped at the actual write, so this becomes fresh evidence.
	close(ct.controlSlot.inbound)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		(&connAttempt{connSlot: &connSlot{id: ConnPoolSize, role: slotRoleControl, stats: stats, inbound: ct.controlSlot.inbound}}).writeGeneration(context.Background(), a, errChanOf(), 0)
	}()
	peer, _ := framing.NewConn(context.Background(), b)
	b.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 256)
	if _, err := peer.Read(buf); err != nil {
		t.Fatalf("clear discarded queued transmission: %v", err)
	}
	<-writerDone
	if got := stats.Snapshot().ProbeState; got != "written_no_reply" {
		t.Fatalf("actual post-clear write evidence %q", got)
	}
}
