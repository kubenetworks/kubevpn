package core

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

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
