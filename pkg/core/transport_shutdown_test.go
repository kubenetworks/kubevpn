package core

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// Observe entry into a blocking select, so cancellation tests cannot pass by
// canceling before the producer starts.
type stage3SelectContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *stage3SelectContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestStage3_BufferedAdmissionCancelKeepsRoute(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	c := &bufferedTCP{Conn: raw, ch: make(chan *Packet, 1), done: make(chan struct{})}
	defer c.Close()
	hub := NewRouteHub()
	ip := net.IPv4(198, 18, 0, 71).To4()
	hub.AddRoute(context.Background(), ip, c)
	queued, p := batchPacket(20, 0), batchPacket(20, 1)
	c.ch <- queued // intentionally no writer: force admission backpressure
	defer func() { drainPacketChan(c.ch); checkBatchRefs(t, []*Packet{queued, p}) }()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &stage3SelectContext{Context: parent, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := hub.WriteToRoutePacket(ctx, string(ip), p); p.release(); done <- err }()
	lifetimeAwait(t, ctx.entered, "blocked buffered admission")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || c.closed.Load() || !hub.HasRoute(string(ip)) {
			t.Fatalf("canceled enqueue evicted healthy route: %v", err)
		}
	case <-time.After(time.Second):
		c.Close()
		<-done
		t.Fatal("buffered route admission ignored producer cancellation")
	}
}

func TestStage3_RawGateCancelKeepsConnection(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	hub := NewRouteHub()
	ip := net.IPv4(198, 18, 0, 72).To4()
	hub.AddRoute(context.Background(), ip, raw)
	entered, release, held := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	go func() {
		hub.WriteFuncToRoute(string(ip), func(net.Conn) error { close(entered); <-release; return nil })
		close(held)
	}()
	lifetimeAwait(t, entered, "raw gate holder")
	p := batchPacket(20, 0)
	defer checkBatchRefs(t, []*Packet{p})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &stage3SelectContext{Context: parent, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := hub.WriteToRoutePacket(ctx, string(ip), p); p.release(); done <- err }()
	lifetimeAwait(t, ctx.entered, "blocked raw gate waiter")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !hub.HasRoute(string(ip)) {
			t.Fatalf("gate cancellation: %v", err)
		}
	case <-time.After(time.Second):
		raw.Close()
		once.Do(func() { close(release) })
		<-done
		<-held
		t.Fatal("raw gate wait ignored cancellation")
	}
	once.Do(func() { close(release) })
	<-held
	go func() { peer.Write([]byte{7}) }()
	var b [1]byte
	if _, err := raw.Read(b[:]); err != nil {
		t.Fatalf("gate waiter closed healthy socket: %v", err)
	}
}

func TestStage3_RawRunningCancelNoReplay(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	fallback, other := net.Pipe()
	defer fallback.Close()
	defer other.Close()
	c := &batchBlockingConn{Conn: raw, entered: make(chan struct{})}
	hub := NewRouteHub()
	ip := net.IPv4(198, 18, 0, 73).To4()
	hub.AddRoute(context.Background(), ip, fallback)
	hub.AddRoute(context.Background(), ip, c)
	p := batchPacket(20, 0)
	defer checkBatchRefs(t, []*Packet{p})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := hub.WriteToRoutePacket(ctx, string(ip), p); p.release(); done <- err }()
	lifetimeAwait(t, c.entered, "raw socket write")
	var prefix [1]byte
	if _, err := io.ReadFull(peer, prefix[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
	case <-time.After(time.Second):
		raw.Close()
		fallback.Close()
		<-done
		t.Fatal("in-progress raw write survived cancellation or replayed")
	}
	members := routeSnapshot(hub)[string(ip)]
	if len(members) != 1 || members[0] != fallback {
		t.Fatal("closed member not retired, or fallback evicted")
	}
}

func TestStage3_ServerOutboundCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dev := &tunDevice{tunInbound: make(chan *Packet)}
	tr := newServerTransport(dev, NewRouteHub())
	done := make(chan struct{})
	go func() { tr.routeOutbound(ctx, config.LPool.Get().([]byte), 20, nil, nil); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		(<-dev.tunInbound).release()
		<-done
		t.Fatal("server outbound enqueue ignored cancellation")
	}
}

func TestStage3_BridgeStopJoinsAdmission(t *testing.T) {
	for _, raceCancel := range []bool{false, true} {
		hub := NewRouteHub()
		var packets []*Packet
		for i := 0; i < MaxSize+32; i++ {
			packets = append(packets, batchPacket(20, i))
		}
		for _, p := range packets[:MaxSize] {
			if !hub.enqueueTUN(context.Background(), p) {
				t.Fatal("initial admission failed")
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		var producers sync.WaitGroup
		for _, p := range packets[MaxSize:] {
			producers.Add(1)
			go func(p *Packet) {
				defer producers.Done()
				if !hub.enqueueTUN(ctx, p) {
					p.release()
				}
			}(p)
		}
		if raceCancel {
			cancel()
		}
		hub.stopTUNBridge()
		cancel()
		producers.Wait()
		late := batchPacket(20, 99)
		packets = append(packets, late)
		if hub.enqueueTUN(context.Background(), late) {
			drainPacketChan(hub.tunQueue) // failure-only recovery, never establishes success
			t.Error("bridge accepted a late packet after stop")
		} else {
			late.release()
		}
		checkBatchRefs(t, packets)
	}
}

type stage3AdmissionContext struct {
	context.Context
	entered chan struct{}
	resume  chan struct{}
}

func (c *stage3AdmissionContext) Done() <-chan struct{} {
	close(c.entered)
	<-c.resume
	return c.Context.Done()
}

func TestStage3_BridgeReadySendRacesStop(t *testing.T) {
	for range 32 {
		hub := NewRouteHub()
		parent, cancel := context.WithCancel(context.Background())
		ctx := &stage3AdmissionContext{Context: parent, entered: make(chan struct{}), resume: make(chan struct{})}
		var resumeOnce sync.Once
		resume := func() { resumeOnce.Do(func() { close(ctx.resume) }) }
		defer resume()
		defer cancel()
		p := batchPacket(20, 0)
		producer, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			if !hub.enqueueTUN(ctx, p) {
				p.release()
			}
			close(producer)
		}()
		lifetimeAwait(t, ctx.entered, "admission past stop check with ready queue")
		go func() { hub.stopTUNBridge(); close(stopped) }()
		lifetimeAwait(t, hub.tunDone, "stop signal before producer join")
		cancel()
		// Both the queue and cancellation are now ready. Either select outcome
		// must leave exactly the caller reference after the exclusive drain.
		resume()
		lifetimeAwait(t, producer, "racing producer")
		lifetimeAwait(t, stopped, "bridge joined racing producer")
		if len(hub.tunQueue) != 0 {
			t.Fatal("racing enqueue escaped final drain")
		}
		checkBatchRefs(t, []*Packet{p})
	}
}

type stage3CloseBarrier struct {
	net.Conn
	entered chan struct{}
	closed  chan struct{}
	resume  chan struct{}
}

func (c *stage3CloseBarrier) Write(p []byte) (int, error) { close(c.entered); return c.Conn.Write(p) }
func (c *stage3CloseBarrier) Close() error {
	err := c.Conn.Close()
	close(c.closed)
	<-c.resume
	return err
}
func TestStage3_RawCancelJoinsCloseAndPreservesReplacement(t *testing.T) {
	raw, peer := net.Pipe()
	defer raw.Close()
	defer peer.Close()
	c := &stage3CloseBarrier{Conn: raw, entered: make(chan struct{}), closed: make(chan struct{}), resume: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(c.resume) })
	hub := NewRouteHub()
	ip := net.IPv4(198, 18, 0, 76).To4()
	hub.AddRoute(context.Background(), ip, c)
	p := batchPacket(20, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := hub.WriteToRoutePacket(ctx, string(ip), p); p.release(); done <- err }()
	lifetimeAwait(t, c.entered, "active raw write")
	cancel()
	lifetimeAwait(t, c.closed, "socket closed outside route metadata locks")
	hub.RemoveRoutesByConn(context.Background(), c)
	fresh, other := net.Pipe()
	defer fresh.Close()
	defer other.Close()
	hub.AddRoute(context.Background(), ip, fresh)
	select {
	case <-done:
		t.Fatal("route returned before cancellation Close callback joined")
	default:
	}
	once.Do(func() { close(c.resume) })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("lost original write error or cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("raw callback join stuck")
	}
	members := routeSnapshot(hub)[string(ip)]
	if len(members) != 1 || members[0] != fresh {
		t.Fatal("cancellation cleanup pruned replacement route")
	}
	checkBatchRefs(t, []*Packet{p})
}

func TestStage3_BridgeFullDeviceQueueCancel(t *testing.T) {
	hub := NewRouteHub()
	dev := &tunDevice{tunOutbound: make(chan *Packet)}
	tr := newServerTransport(dev, hub)
	p := batchPacket(20, 0)
	hub.enqueueTUN(context.Background(), p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.routeTCPToTun(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		(<-dev.tunOutbound).release()
		<-done
		t.Error("bridge device enqueue ignored cancel")
	}
	hub.stopTUNBridge()
	checkBatchRefs(t, []*Packet{p})
}

// stage3FatalTUN waits for a write failure trigger, independently of the parent
// connection context. Closing the device unblocks its read without test drains.
type stage3FatalTUN struct {
	closed  chan struct{}
	writing chan struct{}
	fail    chan struct{}
	once    sync.Once
}

func (c *stage3FatalTUN) Read([]byte) (int, error) { <-c.closed; return 0, net.ErrClosed }
func (c *stage3FatalTUN) Write([]byte) (int, error) {
	close(c.writing)
	select {
	case <-c.fail:
		return 0, io.ErrUnexpectedEOF
	case <-c.closed:
		return 0, net.ErrClosed
	}
}
func (c *stage3FatalTUN) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *stage3FatalTUN) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (c *stage3FatalTUN) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (c *stage3FatalTUN) SetDeadline(time.Time) error      { return nil }
func (c *stage3FatalTUN) SetReadDeadline(time.Time) error  { return nil }
func (c *stage3FatalTUN) SetWriteDeadline(time.Time) error { return nil }

func stage3Wait(t *testing.T, predicate func() bool, what string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !predicate() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timeout: %s", what)
		}
	}
}

func TestStage3_ServeFullQueuesDeviceLocalStop(t *testing.T) {
	for _, mode := range []string{"buffered-fatal", "raw-gate-fatal", "buffered-cancel"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := NewRouteHub()
			ip := net.IPv4(198, 18, 0, 74).To4()
			raw, peer := net.Pipe()
			defer raw.Close()
			defer peer.Close()
			var packets []*Packet
			packet := func() *Packet { p := batchPacket(20, len(packets)); packets = append(packets, p); return p }
			conn := &bufferedTCP{Conn: raw, ch: make(chan *Packet, 1), done: make(chan struct{})}
			var bufferedPacket *Packet
			if mode == "raw-gate-fatal" {
				hub.AddRoute(parent, ip, raw)
			} else {
				bufferedPacket = packet()
				conn.ch <- bufferedPacket
				hub.AddRoute(parent, ip, conn)
			}
			hub.mu.Lock()
			list := hub.routes[string(ip)]
			hub.mu.Unlock()
			gateRelease := func() {}
			if mode == "raw-gate-fatal" {
				if err := list.lockWrite(parent); err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				gateRelease = func() { once.Do(list.unlockWrite) }
				defer gateRelease()
			}
			// Observe route validation while the route writer is still blocked.
			list.mu.Lock()
			var unlockOnce sync.Once
			unlockList := func() { unlockOnce.Do(list.mu.Unlock) }
			defer unlockList()
			tun := &stage3FatalTUN{closed: make(chan struct{}), writing: make(chan struct{}), fail: make(chan struct{})}
			dev := &tunDevice{tun: tun, tunInbound: make(chan *Packet, 1), tunOutbound: make(chan *Packet, 1), errChan: make(chan error, 1)}
			dev.transport = newServerTransport(dev, hub)
			first := packet()
			first.dst = ip
			dev.tunInbound <- first
			dev.tunOutbound <- packet()
			done := make(chan struct{})
			go func() { serve(parent, dev); close(done) }()
			// routeTun consumes before snapshotting under list.mu. This barrier
			// guarantees it has an in-flight packet when we fill tunInbound.
			stage3Wait(t, func() bool { return len(dev.tunInbound) == 0 }, "route consumed first packet")
			unlockList()
			inbound := packet()
			inbound.dst = ip
			dev.tunInbound <- inbound
			lifetimeAwait(t, tun.writing, "blocked device writer")
			dev.tunOutbound <- packet()
			bridge := packet()
			hub.enqueueTUN(parent, bridge)
			stage3Wait(t, func() bool { return len(hub.tunQueue) == 0 }, "bridge blocked on device queue")
			for i := 0; i < cap(hub.tunQueue); i++ {
				hub.enqueueTUN(parent, packet())
			}
			extra := packet()
			external := &stage3SelectContext{Context: parent, entered: make(chan struct{})}
			externalDone := make(chan struct{})
			go func() {
				if !hub.enqueueTUN(external, extra) {
					extra.release()
				}
				close(externalDone)
			}()
			lifetimeAwait(t, external.entered, "external producer at full bridge")
			if mode == "buffered-cancel" {
				cancel()
			} else {
				close(tun.fail)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				// Failure recovery releases gates so a regression cannot poison the suite.
				cancel()
				gateRelease()
				conn.Close()
				raw.Close()
				tun.Close()
				t.Fatal("serve waited for parent connection or full device queues")
			}
			lifetimeAwait(t, externalDone, "external producer rejected after bridge stop")
			if mode != "buffered-cancel" && parent.Err() != nil {
				t.Fatal("fatal test canceled parent")
			}
			if conn.closed.Load() || !hub.HasRoute(string(ip)) {
				t.Fatal("device-local stop retired healthy parent route")
			}
			for _, q := range []chan *Packet{dev.tunInbound, dev.tunOutbound, hub.tunQueue} {
				if len(q) != 0 {
					t.Fatal("serve left queued packets")
				}
			}
			late := packet()
			if hub.enqueueTUN(parent, late) {
				t.Fatal("late admission accepted")
			}
			late.release()
			// The deliberately unconsumed parent connection queue is not owned by
			// serve; inspect every device reference before cleaning that fixture.
			if bufferedPacket != nil {
				if bufferedPacket.refs.Load() != 1 || len(conn.ch) != 1 {
					t.Fatal("device stole parent queue ownership")
				}
				(<-conn.ch).release()
			}
			checkBatchRefs(t, packets)
		})
	}
}

// shutdownBarrierTUN holds a write until serve closes the device. The reader
// remains a producer after Close, modeling an already-completed socket read
// whose canceled select can still choose a ready queue.
type shutdownBarrierTUN struct {
	net.Conn
	writing chan struct{}
	closed  chan struct{}
	late    chan struct{}
	once    sync.Once
}

func (c *shutdownBarrierTUN) Write([]byte) (int, error) {
	close(c.writing)
	<-c.closed
	return 0, io.ErrClosedPipe
}
func (c *shutdownBarrierTUN) Read([]byte) (int, error) { <-c.late; return 0, io.EOF }
func (c *shutdownBarrierTUN) Close() error             { c.once.Do(func() { close(c.closed) }); return nil }

type shutdownBarrierTransport struct {
	*clientTransport
	produce func(context.Context)
	workers []namedRoutine
}

func (t *shutdownBarrierTransport) routines() []namedRoutine {
	return append(t.workers, namedRoutine{"late-reader", t.produce})
}

func TestTransportShutdown_ServeFinalQueueDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun := &shutdownBarrierTUN{writing: make(chan struct{}), closed: make(chan struct{}), late: make(chan struct{})}
	dev := &tunDevice{tun: tun, tunInbound: make(chan *Packet, 8), tunOutbound: make(chan *Packet, 8), errChan: make(chan error, 1)}
	tr := newClientTransport(dev, nil, nil)
	tr.slots = []*connSlot{{inbound: make(chan *Packet, 8)}}
	tr.controlSlot = &connSlot{inbound: make(chan *Packet, 8)}
	var packets []*Packet
	packet := func(size int) *Packet { p := batchPacket(size, len(packets)); packets = append(packets, p); return p }
	// A standalone packet holds the writer so the drain sequence can be seeded.
	dev.tunOutbound <- packet(65537)
	marker := packet(20)
	late := packet(20)
	tr.slots[0].inbound <- packet(20)
	tr.controlSlot.inbound <- packet(20)
	local := packet(20)
	inbound := packet(20)
	produced := make(chan struct{})
	dev.transport = &shutdownBarrierTransport{clientTransport: tr, produce: func(ctx context.Context) {
		<-tun.closed
		// The marker is released by the writer's deferred drain. A nil sentinel
		// immediately after it ends that drain, even if this producer wins scheduling.
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for marker.refs.Load() != 0 {
			select {
			case <-tick.C:
			case <-deadline.C:
				return
			}
		}
		dev.tunOutbound <- late
		tr.gvisorInbound <- local
		dev.tunInbound <- inbound
		close(produced)
		close(tun.late)
	}}
	done := make(chan struct{})
	go func() { serve(ctx, dev); close(done) }()
	select {
	case <-tun.writing:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not start")
	}
	dev.tunOutbound <- marker
	dev.tunOutbound <- nil
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve failed to join late reader")
	}
	select {
	case <-produced:
	default:
		t.Fatal("late enqueue was not exercised")
	}
	checkBatchRefs(t, packets) // caller refs only; no test-side queue drain
	for _, q := range []chan *Packet{dev.tunInbound, dev.tunOutbound, tr.gvisorInbound, tr.slots[0].inbound, tr.controlSlot.inbound} {
		if len(q) != 0 {
			t.Errorf("final queue has %d packets", len(q))
		}
	}
}

func TestTransportShutdown_ServeStoppedPoolAndControlQueues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // exercise startup cancellation before either slot routine dials
	a, b := net.Pipe()
	defer b.Close()
	dev := &tunDevice{tun: a, tunInbound: make(chan *Packet, 8), tunOutbound: make(chan *Packet, 8), errChan: make(chan error, 1)}
	tr := newClientTransport(dev, &Forwarder{}, nil)
	poolDone, controlDone := make(chan struct{}), make(chan struct{})
	data, control := batchPacket(20, 0), batchPacket(20, 1)
	dev.transport = &shutdownBarrierTransport{clientTransport: tr, workers: []namedRoutine{
		{"pool", func(ctx context.Context) { tr.runConnPool(ctx); close(poolDone) }},
		{"control", func(ctx context.Context) { tr.runControlSlot(ctx); close(controlDone) }},
	}, produce: func(context.Context) {
		<-poolDone
		<-controlDone
		// Model the last already-running dispatcher/heartbeat send choosing the
		// ready queue rather than cancellation after the consumers have stopped.
		tr.slots[0].inbound <- data
		tr.controlSlot.inbound <- control
	}}
	done := make(chan struct{})
	go func() { serve(ctx, dev); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("startup shutdown did not join")
	}
	checkBatchRefs(t, []*Packet{data, control})
}

// Cancellation must break a full device->transport queue; serve joins the TUN
// reader, so an uncancellable enqueue would hang NetworkManager.Stop forever.
func TestTransportShutdown_PoolJoinsSharedStack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dev := &tunDevice{tunInbound: make(chan *Packet), tunOutbound: make(chan *Packet)}
	tr := newClientTransport(dev, &Forwarder{}, &HeartbeatStats{})
	done := make(chan struct{})
	go func() { tr.runConnPool(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pool did not join")
	}
	select {
	case <-tr.interClient.done:
	default:
		t.Fatal("shared gvisor output survived pool")
	}
}

func TestTransportShutdown_FullOutboundQueue(t *testing.T) {
	for _, self := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		dev := &tunDevice{tunInbound: make(chan *Packet)}
		tr := newClientTransport(dev, nil, nil)
		tr.gvisorInbound = make(chan *Packet)
		done := make(chan struct{})
		src, dst := net.IPv4(198, 18, 0, 1), net.IPv4(198, 18, 0, 2)
		if self {
			dst = src
		}
		buf := config.LPool.Get().([]byte)
		go func() { tr.routeOutbound(ctx, buf, 20, src, dst); close(done) }()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			// Release the old uncancellable implementation rather than leak the test.
			if self {
				(<-tr.gvisorInbound).release()
			} else {
				(<-dev.tunInbound).release()
			}
			t.Fatal("device outbound enqueue ignored cancellation")
		}
	}
}
