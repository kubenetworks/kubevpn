package tunnel

import (
	"bytes"
	"context"
	framing "github.com/wencaiwulue/kubevpn/v2/pkg/internal/wire"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// A real Forwarder and loopback socket drive each attempt. Only time, wait
// completion, and deliberately injected socket faults are controlled by the test.
// All seams/configuration are installed before run; these tests are not parallel.
type pacingEvent struct {
	kind   string
	delay  time.Duration
	plan   chan pacingPlan
	resume chan struct{}
}
type pacingPlan struct {
	failDial bool
	wrap     func(net.Conn) net.Conn
}
type pacingHarness struct {
	t          *testing.T
	ctx        context.Context
	cancel     context.CancelFunc
	slot       *connSlot
	events     chan pacingEvent
	peers      chan net.Conn
	done       chan struct{}
	clockCalls chan struct{}
	elapsed    atomic.Int64
	accepts    atomic.Int64
}

func newPacingHarness(t *testing.T) *pacingHarness {
	t.Helper()
	backoff, heartbeat := config.SlotReconnectBackoff, config.HeartbeatInterval
	config.SlotReconnectBackoff, config.HeartbeatInterval = 2*time.Second, 5*time.Second
	t.Cleanup(func() { config.SlotReconnectBackoff, config.HeartbeatInterval = backoff, heartbeat })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &pacingHarness{t: t, ctx: ctx, cancel: cancel, events: make(chan pacingEvent, 16), peers: make(chan net.Conn, 16), done: make(chan struct{}), clockCalls: make(chan struct{}, 128)}
	h.slot = &connSlot{stats: &HeartbeatStats{}, inbound: make(chan *packet, 128), tunOutbound: make(chan *packet, 128)}
	h.slot.retryNow = func() time.Time {
		now := time.Unix(1700000000, h.elapsed.Load())
		h.clockCalls <- struct{}{}
		return now
	}
	h.slot.retryWait = func(ctx context.Context, delay time.Duration) bool {
		e := pacingEvent{kind: "wait", delay: delay, resume: make(chan struct{})}
		h.events <- e
		select {
		case <-e.resume:
			return ctx.Err() == nil
		case <-ctx.Done():
			return false
		}
	}
	h.slot.forward = (testDialer{Addr: ln.Addr().String(), MaxRetries: 1, Connector: testConnector{}, Transporter: &mockTransporter{dialFn: func(ctx context.Context, addr string) (net.Conn, error) {
		e := pacingEvent{kind: "dial", plan: make(chan pacingPlan, 1)}
		h.events <- e
		var plan pacingPlan
		select {
		case plan = <-e.plan:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if plan.failDial {
			h.elapsed.Add(int64(10 * time.Second))
			return nil, io.ErrUnexpectedEOF
		}
		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		peer, err := ln.Accept()
		if err != nil {
			raw.Close()
			return nil, err
		}
		h.accepts.Add(1)
		h.peers <- peer
		if plan.wrap != nil {
			raw = plan.wrap(raw)
		}
		return raw, nil
	}}}).config()
	t.Cleanup(func() {
		cancel()
		ln.Close()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Error("slot did not join")
		}
		drainPacketChan(h.slot.inbound)
		drainPacketChan(h.slot.tunOutbound)
	})
	return h
}
func (h *pacingHarness) start() { go func() { defer close(h.done); h.slot.run(h.ctx) }() }
func (h *pacingHarness) next(kind string) pacingEvent {
	h.t.Helper()
	select {
	case e := <-h.events:
		if e.kind != kind {
			h.t.Fatalf("got %s, want %s (post-dial pacing)", e.kind, kind)
		}
		return e
	case <-time.After(3 * time.Second):
		h.t.Fatalf("missing %s completion barrier", kind)
	}
	return pacingEvent{}
}
func (h *pacingHarness) dial(plan pacingPlan) net.Conn {
	h.t.Helper()
	h.next("dial").plan <- plan
	select {
	case peer := <-h.peers:
		h.t.Cleanup(func() { peer.Close() })
		return peer
	case <-time.After(3 * time.Second):
		h.t.Fatal("loopback accept missing")
	}
	return nil
}
func (h *pacingHarness) clock() {
	h.t.Helper()
	select {
	case <-h.clockCalls:
	case <-time.After(3 * time.Second):
		h.t.Fatal("retry clock barrier missing")
	}
}
func (h *pacingHarness) wait(ceiling time.Duration) pacingEvent {
	h.t.Helper()
	e := h.next("wait")
	if e.delay < ceiling/2 || e.delay > ceiling {
		h.t.Fatalf("wait %v outside [%v,%v]", e.delay, ceiling/2, ceiling)
	}
	return e
}
func pacingAwait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("missing %s barrier", what)
	}
}

// Faults occur on real socket wrappers, after Forwarder.DialContext succeeds.
type pacingFaultConn struct {
	net.Conn
	writeFault         bool
	readDeadlineFault  bool
	writeDeadlineFault bool
}

func (c *pacingFaultConn) Write(p []byte) (int, error) {
	if c.writeFault {
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Write(p)
}
func (c *pacingFaultConn) SetReadDeadline(tm time.Time) error {
	if c.readDeadlineFault {
		return io.ErrUnexpectedEOF
	}
	return c.Conn.SetReadDeadline(tm)
}
func (c *pacingFaultConn) SetWriteDeadline(tm time.Time) error {
	if c.writeDeadlineFault {
		return io.ErrUnexpectedEOF
	}
	return c.Conn.SetWriteDeadline(tm)
}

// Hold the error log before notification reaches run. Failure at 1.9s is
// rapid even if accounting/logging/notification completes after the 2s window.
type pacingErrorLog struct {
	match   []byte
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pacingErrorLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, w.match) {
		w.once.Do(func() { close(w.entered); <-w.release })
	}
	return len(p), nil
}

type pacingTimedFault struct {
	net.Conn
	elapsed      *atomic.Int64
	readDeadline bool
}

func (c *pacingTimedFault) Write([]byte) (int, error) {
	c.elapsed.Store(int64(1900 * time.Millisecond))
	return 0, io.ErrUnexpectedEOF
}
func (c *pacingTimedFault) SetReadDeadline(tm time.Time) error {
	if c.readDeadline {
		c.elapsed.Store(int64(1900 * time.Millisecond))
		return io.ErrUnexpectedEOF
	}
	return c.Conn.SetReadDeadline(tm)
}

func TestRetryPacing_FailureBeforeBlockedLog(t *testing.T) {
	for _, readDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "batch-write", true: "read-deadline"}[readDeadline], func(t *testing.T) {
			h := newPacingHarness(t)
			match := "Failed to write to remote"
			if readDeadline {
				match = "Failed to set read deadline"
			}
			logs := &pacingErrorLog{match: []byte(match), entered: make(chan struct{}), release: make(chan struct{})}
			h.ctx = plog.WithLogger(h.ctx, plog.GetLoggerForClient(int32(logrus.ErrorLevel), logs))
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(logs.release) }) })
			h.start()
			h.dial(pacingPlan{}).Close() // first failure arms immediate recovery
			h.clock()
			h.clock()
			h.dial(pacingPlan{wrap: func(c net.Conn) net.Conn {
				return &pacingTimedFault{Conn: c, elapsed: &h.elapsed, readDeadline: readDeadline}
			}})
			h.clock() // successful second dial at zero
			if !readDeadline {
				p := batchPacket(20, 1)
				p.release()
				h.slot.inbound <- p
			}
			pacingAwait(t, logs.entered, "failure log blocked before notification")
			h.elapsed.Store(int64(2100 * time.Millisecond))
			releaseOnce.Do(func() { close(logs.release) })
			h.wait(125 * time.Millisecond) // not an immediate stable reset
			if _, ok := any(h.slot).(interface{ observeFailure() }); ok {
				t.Fatal("attempt observer leaked onto the long-lived slot")
			}
		})
	}
}

// The reader installs its fatal deadline first. Only the writer's subsequent,
// best-effort refresh fails; the same connection must continue carrying data.
type pacingToleratedRefresh struct {
	net.Conn
	reading   chan struct{}
	readOnce  sync.Once
	deadlines atomic.Int32
	failWrite atomic.Bool
}

func (c *pacingToleratedRefresh) Read(p []byte) (int, error) {
	c.readOnce.Do(func() { close(c.reading) })
	return c.Conn.Read(p)
}
func (c *pacingToleratedRefresh) SetReadDeadline(tm time.Time) error {
	if c.deadlines.Add(1) == 2 {
		return io.ErrUnexpectedEOF
	}
	return c.Conn.SetReadDeadline(tm)
}
func (c *pacingToleratedRefresh) Write(p []byte) (int, error) {
	if c.failWrite.Load() {
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Write(p)
}

func TestRetryPacing_ToleratedRefreshDoesNotPoisonStableReset(t *testing.T) {
	for _, terminalWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "write"}[terminalWrite], func(t *testing.T) {
			h := newPacingHarness(t)
			logs := &pacingErrorLog{match: []byte("Failed to refresh read deadline after write"), entered: make(chan struct{}), release: make(chan struct{})}
			h.ctx = plog.WithLogger(h.ctx, plog.GetLoggerForClient(int32(logrus.DebugLevel), logs))
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(logs.release) }) })
			h.start()
			h.dial(pacingPlan{}).Close() // prior rapid streak
			h.clock()
			h.clock()
			raw := &pacingToleratedRefresh{reading: make(chan struct{})}
			peer := h.dial(pacingPlan{wrap: func(c net.Conn) net.Conn { raw.Conn = c; return raw }})
			h.clock()
			pacingAwait(t, raw.reading, "reader installed fatal deadline")
			peer.SetReadDeadline(time.Now().Add(3 * time.Second))
			send := func(seq int) {
				p := batchPacket(20, seq)
				p.release()
				h.slot.inbound <- p
			}
			receive := func(seq int) {
				buf := make([]byte, 20)
				if _, err := io.ReadFull(peer, buf); err != nil {
					t.Fatal(err)
				}
				if buf[0] != 0 || buf[1] != 18 || !bytes.Equal(buf[2:], bytes.Repeat([]byte{byte(seq)}, 18)) {
					t.Fatal("successful batch corrupted")
				}
			}
			h.elapsed.Store(int64(1900 * time.Millisecond))
			send(1)
			receive(1)
			pacingAwait(t, logs.entered, "tolerated refresh failure")
			releaseOnce.Do(func() { close(logs.release) })
			send(2)
			receive(2) // proves the writer continued on the same connection
			h.elapsed.Store(int64(2100 * time.Millisecond))
			if terminalWrite {
				raw.failWrite.Store(true)
				send(3)
			} else {
				peer.Close()
			}
			// Stable terminal failure must clear the prior streak immediately.
			h.dial(pacingPlan{}).Close()
			// The next quick failure is first in a new streak: also immediate.
			h.dial(pacingPlan{}).Close()
			h.wait(125 * time.Millisecond)
		})
	}
}

func TestRetryPacing_RapidPostDialFailures(t *testing.T) {
	for _, mode := range []string{"EOF", "declaration", "registration", "read-deadline", "write-deadline", "write"} {
		t.Run(mode, func(t *testing.T) {
			h := newPacingHarness(t)
			if mode == "declaration" {
				h.slot.isControl = true
			}
			if mode == "registration" {
				h.slot.registrations = func(context.Context) [][]byte { return [][]byte{{packetTypeToGvisor}} }
			}
			h.start()
			for attempt := 0; attempt < 6; attempt++ {
				peer := h.dial(pacingPlan{wrap: func(c net.Conn) net.Conn {
					return &pacingFaultConn{Conn: c, writeFault: mode == "declaration" || mode == "registration" || mode == "write", readDeadlineFault: mode == "read-deadline", writeDeadlineFault: mode == "write-deadline"}
				}})
				if mode == "EOF" {
					peer.Close()
				}
				if mode == "write" || mode == "write-deadline" {
					p := batchPacket(20, attempt)
					p.release() // no retained fixture reference
					h.slot.inbound <- p
				}
				if attempt > 0 {
					close(h.wait(min(125*time.Millisecond<<uint(attempt-1), 500*time.Millisecond)).resume)
				}
			}
		})
	}
}

func TestRetryPacing_StableBoundaryExcludesDialAndCleanup(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(map[bool]string{false: "window-minus-1ns", true: "window-equal"}[stable], func(t *testing.T) {
			h := newPacingHarness(t)
			h.start()
			peer := h.dial(pacingPlan{})
			h.clock() // successful dial return
			peer.Close()
			h.clock() // first rapid failure
			peer = h.dial(pacingPlan{wrap: func(c net.Conn) net.Conn {
				// Time spent dialing cannot count toward the stable window.
				h.elapsed.Add(int64(10 * time.Second))
				return &pacingCloseClock{Conn: c, elapsed: &h.elapsed}
			}})
			h.clock()
			age := 2 * time.Second
			if !stable {
				age--
			}
			h.elapsed.Add(int64(age))
			peer.Close()
			h.clock()
			if !stable {
				close(h.wait(125 * time.Millisecond).resume)
			}
			peer = h.dial(pacingPlan{})
			h.clock()
			peer.Close()
			h.clock()
			if !stable {
				close(h.wait(250 * time.Millisecond).resume)
			}
			peer = h.dial(pacingPlan{})
			h.clock()
			peer.Close()
			h.clock()
			ceiling := 500 * time.Millisecond
			if stable {
				ceiling = 125 * time.Millisecond
			}
			h.wait(ceiling)
		})
	}
}

type pacingCloseClock struct {
	net.Conn
	elapsed *atomic.Int64
}

func (c *pacingCloseClock) Close() error {
	c.elapsed.Add(int64(10 * time.Second))
	return c.Conn.Close()
}

func TestRetryPacing_DialFailureClearsStreak(t *testing.T) {
	h := newPacingHarness(t)
	h.start()
	for i := 0; i < 2; i++ {
		h.dial(pacingPlan{}).Close()
	}
	close(h.wait(125 * time.Millisecond).resume)
	h.next("dial").plan <- pacingPlan{failDial: true}
	e := h.next("wait")
	if e.delay != 2*time.Second {
		t.Fatalf("dial failure wait=%v, want existing 2s", e.delay)
	}
	close(e.resume)
	for i := 0; i < 2; i++ {
		h.dial(pacingPlan{}).Close()
	}
	h.wait(125 * time.Millisecond)
}

func TestRetryPacing_GenerationDuringWaitAndCancel(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "generation", true: "cancel"}[cancelWait], func(t *testing.T) {
			h := newPacingHarness(t)
			h.start()
			for i := 0; i < 4; i++ {
				h.dial(pacingPlan{}).Close()
				if i > 0 {
					e := h.wait(125 * time.Millisecond << uint(i-1))
					if i < 3 {
						close(e.resume)
						continue
					}
					if cancelWait {
						h.cancel()
						pacingAwait(t, h.done, "canceled wait join")
						if h.accepts.Load() != 4 || len(h.events) != 0 {
							t.Fatal("cancel allowed another dial/accept")
						}
						return
					}
					h.slot.stats.BeginGeneration()
					close(e.resume)
				}
			}
			for i := 0; i < 2; i++ {
				h.dial(pacingPlan{}).Close()
			}
			h.wait(125 * time.Millisecond)
		})
	}
}

// Successful startup/control frames and a real correlated echo are not a
// stability signal. Keep the clock fixed while real protocol traffic completes.
func TestRetryPacing_SuccessfulTrafficDoesNotReset(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(map[bool]string{false: "registration-echo", true: "declaration-write-probe"}[control], func(t *testing.T) {
			h := newPacingHarness(t)
			wire := newProbeWire(t, h.slot.stats)
			payload := (&clientTransport{dev: &tunDevice{addrsFn: func() (net.IP, net.IP, net.IP) {
				return net.ParseIP(regTestTunIPv4), nil, nil
			}}}).registrationPayloads(h.ctx)[0]
			h.slot.isControl = control
			if !control {
				h.slot.registrations = func(context.Context) [][]byte { return [][]byte{payload} }
			}
			h.start()
			for attempt := 0; attempt < 4; attempt++ {
				peer := h.dial(pacingPlan{})
				peer.SetReadDeadline(time.Now().Add(3 * time.Second))
				framed, _ := framing.NewConn(h.ctx, peer)
				buf := make([]byte, 256)
				n, err := framed.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				if control {
					if n != 1 || buf[0] != packetTypeControl {
						t.Fatal("missing initial declaration")
					}
					p := batchPacket(datagramHeaderLen+len(payload), 0)
					copy(p.data[datagramHeaderLen:], payload)
					p.data[datagramHeaderLen] = packetTypeControl
					p.release() // queue owns the only reference
					h.slot.inbound <- p
					n, err = framed.Read(buf)
					if err != nil {
						t.Fatal(err)
					}
				}
				// Deliver on a real framed data connection, just as replies to
				// the control slot arrive on the separate data path in production.
				wire.deliver(wire.echo(buf[typePrefixLen:n]))
				peer.Close()
				if attempt > 0 {
					close(h.wait(125 * time.Millisecond << uint(attempt-1)).resume)
				}
			}
			h.next("dial") // previous writer joined, including finishProbe
			if got := h.slot.stats.Snapshot().Replies; got != 4 {
				t.Fatalf("correlated successful replies=%d, want 4", got)
			}
		})
	}
}

func TestRetryPacing_RetiredFailureIgnoredBeforeWait(t *testing.T) {
	h := newPacingHarness(t)
	h.start()
	h.dial(pacingPlan{}).Close()
	// The raw EOF is observed in the old generation; retirement occurs during
	// framing cleanup, before the slot can decide whether to wait.
	h.dial(pacingPlan{wrap: func(c net.Conn) net.Conn {
		return &pacingRetireConn{Conn: c, stats: h.slot.stats}
	}}).Close()
	for i := 0; i < 2; i++ {
		h.dial(pacingPlan{}).Close()
	}
	h.wait(125 * time.Millisecond)
}

type pacingRetireConn struct {
	net.Conn
	stats *HeartbeatStats
	once  sync.Once
}

func (c *pacingRetireConn) Close() error {
	c.once.Do(func() { c.stats.BeginGeneration() })
	return c.Conn.Close()
}

func TestRetryPacing_TinyIntervals(t *testing.T) {
	for _, interval := range []time.Duration{0, -1, 1, 16} {
		t.Run(interval.String(), func(t *testing.T) {
			h := newPacingHarness(t)
			config.SlotReconnectBackoff, config.HeartbeatInterval = interval, interval
			h.start()
			for i := 0; i < 4; i++ {
				h.dial(pacingPlan{}).Close()
				if interval >= 16 && i > 0 {
					close(h.wait(time.Nanosecond).resume)
				}
			}
			h.cancel()
			pacingAwait(t, h.done, "tiny interval cancellation")
		})
	}
}

// A read failure has already been observed, but the old writer cannot return
// until released. Neither retry wait nor the next Dial may precede that join.
type pacingHeldWriter struct {
	net.Conn
	entered chan struct{}
	closed  chan struct{}
	release chan struct{}
	once    sync.Once
	exited  atomic.Bool
}

func (c *pacingHeldWriter) Write([]byte) (int, error) {
	close(c.entered)
	<-c.release
	c.exited.Store(true)
	return 0, io.ErrUnexpectedEOF
}
func (c *pacingHeldWriter) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}

func TestRetryPacing_JoinsOldWriterPreservesQueue(t *testing.T) {
	h := newPacingHarness(t)
	held := &pacingHeldWriter{entered: make(chan struct{}), closed: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(held.release) }) })
	wait := h.slot.retryWait
	h.slot.retryWait = func(ctx context.Context, delay time.Duration) bool {
		if !held.exited.Load() {
			t.Error("wait started before old writer returned")
		}
		return wait(ctx, delay)
	}
	h.start()
	h.dial(pacingPlan{}).Close() // arm the streak, immediate first retry
	peer := h.dial(pacingPlan{wrap: func(c net.Conn) net.Conn { held.Conn = c; return held }})
	first := batchPacket(20, 0)
	h.slot.inbound <- first
	pacingAwait(t, held.entered, "old writer")
	var suffix []*packet
	for i := 1; i <= 5; i++ {
		p := batchPacket(20, i)
		suffix = append(suffix, p)
		h.slot.inbound <- p
	}
	peer.Close()
	pacingAwait(t, held.closed, "read failure cleanup")
	if len(h.events) != 0 || h.accepts.Load() != 2 {
		t.Fatal("retry began before old writer exit")
	}
	if len(h.slot.inbound) != len(suffix) {
		t.Fatal("cleanup consumed untouched suffix")
	}
	once.Do(func() { close(held.release) })
	close(h.wait(125 * time.Millisecond).resume)
	peer = h.dial(pacingPlan{})
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i, p := range suffix {
		buf := make([]byte, 20)
		if _, err := io.ReadFull(peer, buf); err != nil {
			t.Fatal(err)
		}
		for j, b := range p.data[:20] {
			if buf[j] != b {
				t.Fatalf("suffix %d corrupt/replayed at byte %d", i, j)
			}
		}
	}
	h.cancel()
	pacingAwait(t, h.done, "replacement writer join")
	var b [1]byte
	if n, err := peer.Read(b[:]); n != 0 || err != io.EOF {
		t.Fatalf("extra suffix replay: %d %v", n, err)
	}
	checkBatchRefs(t, append([]*packet{first}, suffix...))
}
