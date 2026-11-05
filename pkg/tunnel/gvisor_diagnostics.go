package tunnel

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// Order is fixed, with resets last: resets provide context, not an anomaly trigger.
var nativeCounterNames = [...]string{
	"tcp_retransmits", "tcp_timeouts", "tcp_forward_max_in_flight_drop",
	"ip_malformed_packets_received", "ip_outgoing_packet_errors",
	"udp_receive_buffer_errors", "udp_malformed_packets_received", "udp_checksum_errors", "udp_packet_send_errors",
	"dropped_packets", "tcp_resets_sent", "tcp_resets_received",
}

type gvisorSnapshot struct {
	counters               [12]uint64
	established, connected uint64
	queued                 int
}

func nativeSnapshot(s tcpip.Stats, ep *channel.Endpoint) gvisorSnapshot {
	return gvisorSnapshot{
		counters: [12]uint64{
			s.TCP.Retransmits.Value(), s.TCP.Timeouts.Value(), s.TCP.ForwardMaxInFlightDrop.Value(),
			s.IP.MalformedPacketsReceived.Value(), s.IP.OutgoingPacketErrors.Value(),
			s.UDP.ReceiveBufferErrors.Value(), s.UDP.MalformedPacketsReceived.Value(), s.UDP.ChecksumErrors.Value(), s.UDP.PacketSendErrors.Value(),
			s.DroppedPackets.Value(), s.TCP.ResetsSent.Value(), s.TCP.ResetsReceived.Value(),
		},
		established: s.TCP.CurrentEstablished.Value(), connected: s.TCP.CurrentConnected.Value(), queued: ep.NumQueued(),
	}
}

var nextGvisorDiagnosticID atomic.Uint64

// The registry holds sources, not stacks. Stats is a shallow copy of atomic
// counter pointers; only the numeric snapshot crosses into a log record.
// mu never spans logging, stack destruction, or business/route locks.
type gvisorDiagnostic struct {
	owner                   *datapathDiagnostics
	ctx                     context.Context
	id                      uint64
	role, peer              string
	capacity                int
	closed                  atomic.Bool
	closeOnce               sync.Once
	mu                      sync.Mutex
	stats                   tcpip.Stats
	endpoint                *channel.Endpoint
	baseline                gvisorSnapshot
	pending                 [12]uint64
	windowStart, lastReport time.Time
	lastSample              time.Time
}

func diagnosticGvisor(ctx context.Context, s *stack.Stack, ep *channel.Endpoint, capacity int, role string, peer net.IP) *gvisorDiagnostic {
	d, _ := ctx.Value(datapathDiagnosticKey{}).(*datapathDiagnostics)
	if d == nil || ctx.Err() != nil {
		return nil
	}
	g := &gvisorDiagnostic{owner: d, ctx: ctx, id: nextGvisorDiagnosticID.Add(1), role: role, capacity: capacity, stats: s.Stats(), endpoint: ep, windowStart: time.Now()}
	if len(peer) > 0 {
		g.peer = peer.String()
	}
	g.baseline = nativeSnapshot(g.stats, ep)
	g.lastSample = g.windowStart
	d.mu.Lock()
	// Cancellation may race construction/publication. No late registry reference.
	if ctx.Err() != nil {
		d.mu.Unlock()
		g.close()
		return nil
	}
	d.native = append(d.native, g)
	d.mu.Unlock()
	return g
}

func (g *gvisorDiagnostic) close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		g.closed.Store(true)
		g.owner.mu.Lock()
		for i, p := range g.owner.native {
			if p == g {
				copy(g.owner.native[i:], g.owner.native[i+1:])
				g.owner.native[len(g.owner.native)-1] = nil
				g.owner.native = g.owner.native[:len(g.owner.native)-1]
				break
			}
		}
		g.owner.mu.Unlock()
		g.mu.Lock()
		g.endpoint = nil
		g.stats = tcpip.Stats{}
		g.mu.Unlock()
	})
}

// accumulate rebases each decreasing counter independently. The report window
// is separate from the last sample; suppressed anomalies survive quiet/gap ticks.
func (g *gvisorDiagnostic) accumulate(current gvisorSnapshot) {
	for i, value := range current.counters {
		if value >= g.baseline.counters[i] {
			g.pending[i] += value - g.baseline.counters[i]
		}
	}
	g.baseline = current
}

func (g *gvisorDiagnostic) sample(now time.Time, allowed bool) (emitted, omitted bool) {
	if g.ctx.Err() != nil {
		g.close()
		return false, false
	}
	g.mu.Lock()
	// A queued tick may predate registration or a more recent observation.
	// Do not consume counters or move the accumulation window backward.
	if g.closed.Load() || now.Before(g.lastSample) || now.Before(g.windowStart) {
		g.mu.Unlock()
		return false, false
	}
	g.lastSample = now
	current := nativeSnapshot(g.stats, g.endpoint)
	g.accumulate(current)
	anomaly := false
	for _, n := range g.pending[:10] {
		anomaly = anomaly || n > 0
	}
	if !anomaly {
		// RST-only intervals remain silent, and do not extend an anomaly window.
		g.pending = [12]uint64{}
		g.windowStart = now
		g.mu.Unlock()
		return false, false
	}
	if !g.lastReport.IsZero() && now.Sub(g.lastReport) < 30*time.Second {
		g.mu.Unlock()
		return false, false
	}
	if !allowed {
		g.mu.Unlock()
		return false, true
	}
	pending, start := g.pending, g.windowStart
	g.pending = [12]uint64{}
	g.windowStart, g.lastReport = now, now
	g.mu.Unlock()
	// Neither unregister nor stack destruction waits for a slow logger. Recheck
	// cancellation immediately before emission; teardown counters are not sampled.
	if g.closed.Load() || g.ctx.Err() != nil {
		return false, false
	}
	var fields strings.Builder
	for i, n := range pending {
		if n > 0 {
			fmt.Fprintf(&fields, " %s_delta=%d", nativeCounterNames[i], n)
		}
	}
	if g.peer != "" {
		fmt.Fprintf(&fields, " peer_tun_ip=%s", g.peer)
	}
	if g.closed.Load() || g.ctx.Err() != nil {
		return false, false
	}
	plog.G(g.ctx).Warnf("gvisor_stats stack_id=%d role=%s scope=stack window=%s%s tcp_current_established=%d tcp_current_connected=%d outbound_queue_depth=%d outbound_queue_capacity=%d", g.id, g.role, now.Sub(start), fields.String(), current.established, current.connected, current.queued, g.capacity)
	return true, false
}
