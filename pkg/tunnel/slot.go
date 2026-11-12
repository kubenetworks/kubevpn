package tunnel

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

type slotRole uint8

const (
	slotRoleData slotRole = iota
	slotRoleControl
)

func (r slotRole) isControl() bool { return r == slotRoleControl }

func (r slotRole) String() string {
	if r == slotRoleControl {
		return "control"
	}
	return "data"
}

// connSlot is one connection in the client pool. It owns its inbound packet channel and the
// dial/reconnect lifecycle, and runs the framed read/write loops over the connection. It
// replaces the previous slotID-threaded free functions.
type connSlot struct {
	id          int
	inbound     chan *packet
	tunOutbound chan *packet
	forward     *DialConfig
	// stats records data-plane liveness from observed heartbeat echo replies; may be nil.
	stats *HeartbeatStats
	// registrations returns the route-registration payloads to announce on this conn after each
	// (re)connect, so the server registers the route immediately instead of waiting for data or a
	// periodic heartbeat. Computed lazily (picks up the current TUN IP); may be nil in tests.
	registrations func(ctx context.Context) [][]byte
	// interClient is the shared transport-level inter-client gvisor stack. readFromConn injects
	// type == packetTypeToGvisor packets directly into it (InjectIP) rather than into a per-slot
	// stack, so the stack outlives any single slot. nil in tests that drive a slot in isolation.
	interClient *interClientStack
	// role is immutable after construction. Control slots send a packetTypeControl
	// declaration so the server handles them separately (no Router registration).
	role slotRole
	// Retry timing seams are set only before run starts; nil uses real time.
	retryNow  func() time.Time
	retryWait func(context.Context, time.Duration) bool
}

// run manages this slot's single connection with reconnect logic.
func (s *connSlot) run(ctx context.Context) {
	now := s.retryNow
	if now == nil {
		now = time.Now
	}
	wait := s.retryWait
	if wait == nil {
		wait = func(ctx context.Context, delay time.Duration) bool {
			if delay <= 0 {
				return ctx.Err() == nil
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
				return ctx.Err() == nil
			case <-ctx.Done():
				return false
			}
		}
	}
	rapidWindow := config.SlotReconnectBackoff
	capDelay := min(rapidWindow/4, config.HeartbeatInterval/10)
	baseDelay := min(rapidWindow/16, capDelay)
	var ceiling time.Duration
	retryGeneration := s.stats.Generation()
	for ctx.Err() == nil {
		generation := s.stats.Generation()
		if generation != retryGeneration {
			ceiling = 0
			retryGeneration = generation
		}
		a := &connAttempt{connSlot: s, now: now}
		a.run(ctx, generation)
		if ctx.Err() != nil {
			return
		}
		if generation != s.stats.Generation() {
			ceiling = 0
			continue
		}
		if a.dialFailure {
			// Dial failures keep their existing backoff, never both delays.
			ceiling = 0
			if !wait(ctx, rapidWindow) {
				return
			}
			continue
		}
		if !a.postDialFailure {
			continue
		}
		// This measures churn, not health: successful registration, writes and
		// probes do not reset it. Dial and cancel/Close/join time do not count.
		if a.failedAt.Sub(a.connectedAt) >= rapidWindow {
			ceiling = 0
			continue
		}
		if ceiling <= 0 {
			ceiling = baseDelay // first rapid failure recovers immediately
			continue
		}
		delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling-ceiling/2)+1))
		// Saturate without overflowing when tests use unusual timing values.
		ceiling += min(ceiling, capDelay-ceiling)
		if !wait(ctx, delay) {
			return
		}
	}
}

// periodicDeadline refreshes a connection deadline lazily — only once less than half the
// timeout remains — to avoid a SetDeadline syscall on every packet. The first refresh() always
// sets the deadline.
type periodicDeadline struct {
	next    time.Time
	timeout time.Duration
	set     func(time.Time) error
}

func (p *periodicDeadline) refresh() error {
	if !p.next.IsZero() && time.Until(p.next) >= p.timeout/2 {
		return nil
	}
	p.next = time.Now().Add(p.timeout)
	return p.set(p.next)
}

// trySendToSlot delivers packet to a slot's inbound channel, dropping (and releasing) it if the
// channel is full so a slow slot cannot stall the others. Returns false if the packet was dropped.
func trySendToSlot(inbound chan *packet, packet *packet) bool {
	select {
	case inbound <- packet:
		return true
	default:
		packet.release()
		return false
	}
}
