package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	dropRouteMissing = iota
	dropRoutesFailed
	dropInvalid
	dropOversized
)

var dropReasonNames = [...]string{"route_missing", "all_routes_failed", "invalid", "oversized"}

const maxDropPeers = 128

type datapathDropPeer struct {
	pending [4]uint64
	last    [4]time.Time
}

type datapathDrops struct {
	mu           sync.Mutex
	counts       [4]uint64
	peers        map[netip.Addr]datapathDropPeer
	overflow     [4]uint64
	overflowLast time.Time
}

func (d *datapathDrops) report(ctx context.Context, reason int, peer net.IP) {
	if ctx.Err() != nil {
		return
	}
	addr, _ := netip.AddrFromSlice(peer)
	addr = addr.Unmap()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counts[reason]++
	if d.peers == nil {
		d.peers = make(map[netip.Addr]datapathDropPeer)
	}
	entry, exists := d.peers[addr]
	if !exists && len(d.peers) >= maxDropPeers {
		d.overflow[reason]++
		return
	}
	entry.pending[reason]++
	d.peers[addr] = entry
}

// Only the owner sampler emits drop logs. Per-peer pending counts are retained
// across quiet periods, and overflow is unattributed rather than blamed on a peer.
func (d *datapathDrops) flush(ctx context.Context, now time.Time) {
	type report struct {
		peer   netip.Addr
		reason int
		count  uint64
	}
	var reports [16]report
	n := 0
	d.mu.Lock()
	for addr, entry := range d.peers {
		quiet := true
		for reason, count := range entry.pending {
			if count > 0 && n < len(reports) && (entry.last[reason].IsZero() || now.Sub(entry.last[reason]) >= 30*time.Second) {
				reports[n] = report{addr, reason, count}
				n++
				entry.pending[reason] = 0
				entry.last[reason] = now
			}
			if entry.pending[reason] > 0 || now.Sub(entry.last[reason]) < 30*time.Second {
				quiet = false
			}
		}
		if quiet {
			delete(d.peers, addr)
		} else {
			d.peers[addr] = entry
		}
	}
	var overflow [4]uint64
	if now.Sub(d.overflowLast) >= 30*time.Second && d.overflow != ([4]uint64{}) {
		overflow = d.overflow
		d.overflow = [4]uint64{}
		d.overflowLast = now
	}
	d.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	for _, r := range reports[:n] {
		plog.G(ctx).Warnf("datapath_drop reason=%s dropped_delta=%d peer_tun_ip=%s", dropReasonNames[r.reason], r.count, r.peer)
	}
	for reason, count := range overflow {
		if count > 0 {
			plog.G(ctx).Warnf("datapath_drop reason=%s dropped_delta=%d peer_tun_ip=untracked peer_limit=%d", dropReasonNames[reason], count, maxDropPeers)
		}
	}
}
func (h *peerRegistry) reportRouteDrop(ctx context.Context, err error, peer net.IP) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	reason := dropRoutesFailed
	if errors.Is(err, errNoRoute) {
		reason = dropRouteMissing
	}
	h.drops.report(ctx, reason, peer)
}

// Diagnostics have their own short state locks. Neither the sampler nor a writer
// holds these locks across packet I/O, route transactions, or logging.
type datapathDiagnostics struct {
	mu                  sync.Mutex
	stages              []*datapathStage
	native              []*gvisorDiagnostic
	previous            time.Time // sampler-owned, like rotation
	rotation            int
	drops               *datapathDrops // installed before sampler startup
	interval, threshold time.Duration  // immutable after startup; test seam
}
type datapathDiagnosticKey struct{}

func newDatapathDiagnostics() *datapathDiagnostics {
	return &datapathDiagnostics{interval: time.Second, threshold: 5 * time.Second}
}
func withDatapathDiagnostics(ctx context.Context, d *datapathDiagnostics) context.Context {
	return context.WithValue(ctx, datapathDiagnosticKey{}, d)
}
func diagnosticStage(ctx context.Context, name, role string, slot int, generation uint64) *datapathStage {
	d, _ := ctx.Value(datapathDiagnosticKey{}).(*datapathDiagnostics)
	if d == nil {
		return nil
	}
	s := &datapathStage{owner: d, ctx: ctx, name: name, role: role, slot: slot, generation: generation}
	// Dispatcher queues outlive transport generations; do not attribute their
	// backlog to the generation that happened to exist at routine startup.
	if slot < 0 || name == "slot_enqueue" || name == "tun_enqueue" {
		s.scope = "scope=session"
	} else {
		s.scope = "generation=" + strconv.FormatUint(generation, 10)
	}
	d.mu.Lock()
	d.stages = append(d.stages, s)
	d.mu.Unlock()
	return s
}

type datapathStage struct {
	owner                            *datapathDiagnostics
	ctx                              context.Context
	name, role                       string
	scope                            string
	peer                             net.IP
	slot                             int
	generation                       uint64
	mu                               sync.Mutex
	token, completed, packets, bytes uint64
	queue                            chan<- *packet
	inflightPackets, inflightBytes   int
	lastSuccess                      time.Time
	active                           bool
	closed                           bool
	start                            time.Time
	// Separate linked waiters preserve each blocked producer's evidence. They are
	// used only after an immediate channel admission failed, never on the fast path.
	waits              *datapathWait
	warned             bool
	lastWarn, baseline time.Time
	observedToken      uint64
	// Active-operation recovery belongs to the warned token, not later progress.
	warnedToken     uint64
	warnedSucceeded bool
	waitEpisode     bool
	warnedWaits     int
	waitFailed      bool
}
type datapathWait struct {
	next   *datapathWait
	start  time.Time
	warned bool
}

var datapathWaitPool = sync.Pool{New: func() any { return new(datapathWait) }}

func (s *datapathStage) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	d := s.owner
	d.mu.Lock()
	for i, p := range d.stages {
		if p == s {
			copy(d.stages[i:], d.stages[i+1:])
			d.stages[len(d.stages)-1] = nil
			d.stages = d.stages[:len(d.stages)-1]
			break
		}
	}
	d.mu.Unlock()
}
func (s *datapathStage) setPeer(ip net.IP) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.peer.Equal(ip) {
		s.peer = append(net.IP(nil), ip...)
	}
	s.mu.Unlock()
}
func (s *datapathStage) progress(packets, bytes int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastSuccess = time.Now()
	s.completed++
	s.packets += uint64(packets)
	s.bytes += uint64(bytes)
	s.mu.Unlock()
}
func (s *datapathStage) begin(work ...int) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token++
	s.active = true
	s.start = time.Now()
	s.inflightPackets, s.inflightBytes = 0, 0
	if len(work) == 2 {
		s.inflightPackets, s.inflightBytes = work[0], work[1]
	}
	return s.token
}
func (s *datapathStage) setQueue(q chan<- *packet) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.queue = q
	s.mu.Unlock()
}
func (s *datapathStage) finish(token uint64, packets, bytes int, success bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if token != s.token {
		return
	}
	s.active = false
	s.inflightPackets, s.inflightBytes = 0, 0
	if s.warned && !s.waitEpisode && token == s.warnedToken {
		s.warnedSucceeded = success
		if !success {
			s.warned = false
			s.warnedToken = 0
		}
	}
	if success {
		s.lastSuccess = time.Now()
		s.completed++
		s.packets += uint64(packets)
		s.bytes += uint64(bytes)
	}
}
func (s *datapathStage) wait() *datapathWait {
	if s == nil {
		return nil
	}
	w := datapathWaitPool.Get().(*datapathWait)
	w.start = time.Now()
	s.mu.Lock()
	w.next = s.waits
	s.waits = w
	s.mu.Unlock()
	return w
}
func (s *datapathStage) waited(w *datapathWait, success bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := &s.waits; *p != nil; p = &(*p).next {
		if *p == w {
			*p = w.next
			break
		}
	}
	if w.warned {
		s.warnedWaits--
		if !success {
			s.waitFailed = true
		}
		w.warned = false
	}
	w.next = nil
	datapathWaitPool.Put(w)
	if success {
		s.lastSuccess = time.Now()
		s.completed++
	}
}
func diagnosticEnqueue(ctx context.Context, ch chan<- *packet, p *packet, s *datapathStage) bool {
	select {
	case ch <- p:
		return true
	case <-ctx.Done():
		return false
	default:
	}
	s.setQueue(ch)
	w := s.wait()
	ok := false
	defer func() { s.waited(w, ok) }()
	select {
	case ch <- p:
		ok = true
		return true
	case <-ctx.Done():
		return false
	}
}
func batchDiagnosticBytes(ps []*packet) int {
	n := 0
	for _, p := range ps {
		n += p.length
	}
	return n
}

func (d *datapathDiagnostics) run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	d.previous = time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			d.sampleAt(ctx, time.Now(), time.Now)
		}
	}
}

// sample provides a deterministic observation time for serial tests.
func (d *datapathDiagnostics) sample(ctx context.Context, now time.Time) {
	d.sampleAt(ctx, now, nil)
}

// The owner refreshes native observation time after any earlier blocking logs.
func (d *datapathDiagnostics) sampleAt(ctx context.Context, now time.Time, nativeNow func() time.Time) {
	if ctx.Err() != nil {
		return
	}
	if d.drops != nil {
		d.drops.flush(ctx, now)
	}
	gap := !d.previous.IsZero() && now.Sub(d.previous) > max(3*d.interval, d.threshold)
	d.previous = now
	d.mu.Lock()
	stages := append([]*datapathStage(nil), d.stages...)
	native := append([]*gvisorDiagnostic(nil), d.native...)
	d.mu.Unlock()
	if gap {
		plog.G(ctx).Info("datapath observation_gap: sampling baseline reset")
	}
	emitted, omitted := 0, 0
	total := len(stages) + len(native)
	for j := 0; j < total; j++ {
		i := (j + d.rotation) % total
		if i >= len(stages) {
			observed := now
			if nativeNow != nil {
				observed = nativeNow()
			}
			reported, suppressed := native[i-len(stages)].sample(observed, emitted < 16)
			if reported {
				emitted++
			}
			if suppressed {
				omitted++
			}
			continue
		}
		s := stages[i]
		if s.ctx.Err() != nil {
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		start := s.start
		active := s.active
		for w := s.waits; w != nil; w = w.next {
			if !active || w.start.Before(start) {
				start = w.start
			}
			active = true
		}
		if gap {
			s.baseline = now
			s.warned = false
			s.warnedToken, s.warnedSucceeded = 0, false
			s.waitEpisode, s.waitFailed, s.warnedWaits = false, false, 0
			for w := s.waits; w != nil; w = w.next {
				w.warned = false
			}
		}
		// Cancellation/failure of any warned producer terminates that episode
		// without recovery, even if another producer made progress.
		if s.waitEpisode && s.warnedWaits == 0 && s.waitFailed {
			s.warned, s.waitEpisode, s.waitFailed = false, false, false
		}
		if start.Before(s.baseline) {
			start = s.baseline
		}
		warn := active && now.Sub(start) >= d.threshold && (!s.warned || now.Sub(s.lastWarn) >= 30*time.Second)
		recovered := s.warned && (!active || now.Sub(start) < d.threshold) && s.completed > s.observedToken
		if s.waitEpisode {
			recovered = recovered && s.warnedWaits == 0 && !s.waitFailed
		} else {
			recovered = recovered && s.warnedSucceeded
		}
		duration := now.Sub(start)
		completed, packets, bytes := s.completed, s.packets, s.bytes
		completedDelta := completed - s.observedToken
		peer := s.peer
		depth, capacity := len(s.queue), cap(s.queue)
		inPackets, inBytes := s.inflightPackets, s.inflightBytes
		successAge := "never"
		if !s.lastSuccess.IsZero() {
			successAge = now.Sub(s.lastSuccess).String()
		}
		if (warn || recovered) && emitted >= 16 {
			omitted++
			s.mu.Unlock()
			continue
		}
		if warn {
			s.warned = true
			s.lastWarn = now
			s.observedToken = s.completed
			s.warnedToken, s.warnedSucceeded = s.token, false
			for w := s.waits; w != nil; w = w.next {
				if !w.warned {
					w.warned = true
					s.waitEpisode = true
					s.warnedWaits++
				}
			}
		}
		if recovered {
			s.warned = false
			s.warnedToken, s.warnedSucceeded = 0, false
			s.waitEpisode = false
		}
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			continue
		}
		if warn {
			plog.G(s.ctx).Warnf("datapath_stall stage=%s role=%s slot=%d %s active_for=%s completed=%d completed_delta=%d packets=%d bytes=%d peer_tun_ip=%s queue_depth=%d queue_capacity=%d inflight_packets=%d inflight_bytes=%d last_success_age=%s", s.name, s.role, s.slot, s.scope, duration, completed, completedDelta, packets, bytes, peer, depth, capacity, inPackets, inBytes, successAge)
			emitted++
		}
		if recovered {
			plog.G(s.ctx).Infof("datapath_recovered stage=%s role=%s slot=%d %s completed=%d packets=%d bytes=%d", s.name, s.role, s.slot, s.scope, completed, packets, bytes)
			emitted++
		}
	}
	if omitted > 0 {
		plog.G(ctx).Warnf("datapath_stall omitted=%d", omitted)
	}
	if total > 0 {
		d.rotation = (d.rotation + 16) % total
	}
}
