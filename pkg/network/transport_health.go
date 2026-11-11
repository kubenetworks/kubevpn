package network

import (
	"context"
	"runtime"
	"time"
)

// generationHealth evaluates one active generation synchronously. It can request
// fresh heartbeat evidence, but only the supervisor may publish or retire transport.
type generationHealth struct {
	nm              *transportSupervisor
	generation      *transportGeneration
	primed          bool
	armedAt         time.Time
	probeRequested  time.Time
	probeDeadline   <-chan time.Time
	probeTimer      *time.Timer
	networkDebounce <-chan time.Time
	networkTimer    *time.Timer
}

func (nm *transportSupervisor) monitorTransport(ctx context.Context, g *transportGeneration) (bool, error) {
	health := &generationHealth{nm: nm, generation: g}
	return health.run(ctx)
}

func (h *generationHealth) run(ctx context.Context) (bool, error) {
	ticker := time.NewTicker(livenessCheckInterval)
	defer ticker.Stop()
	defer stopHealthTimer(&h.probeTimer)
	defer stopHealthTimer(&h.networkTimer)
	var done [3]<-chan struct{}
	for i, handle := range h.generation.handles {
		done[i] = handle.Done()
	}
	for {
		if terminal := h.nm.terminalCause(); terminal != nil {
			return h.primed, terminal
		}
		for _, handle := range h.generation.handles {
			select {
			case <-handle.Done():
				return h.primed, handle.Err()
			default:
			}
		}
		h.nm.mu.Lock()
		armed, terminal := h.nm.armed, h.nm.err
		h.nm.mu.Unlock()
		if terminal != nil {
			return h.primed, terminal
		}
		if armed && h.armedAt.IsZero() {
			h.armedAt = time.Now()
			h.nm.heartbeatStats.RequestProbe()
		}
		if !h.armedAt.IsZero() {
			last := h.nm.heartbeatStats.LastReply()
			if h.nm.lastReply != nil {
				last = h.nm.lastReply()
			}
			if last.After(h.armedAt) {
				if !h.primed && ctx.Err() == nil {
					h.generation.diagnostic.recovered(ctx, h.generation, h.nm.heartbeatStats.Snapshot())
				}
				h.primed = true
			}
			if !h.probeRequested.IsZero() && last.After(h.probeRequested) {
				h.probeRequested = time.Time{}
				h.probeDeadline = nil
				stopHealthTimer(&h.probeTimer)
			}
			probeExpired := !h.probeRequested.IsZero() && time.Since(h.probeRequested) >= livenessSteadyThreshold
			if probeExpired || (h.probeRequested.IsZero() && ((!h.primed && time.Since(h.armedAt) >= livenessStartupDeadline) || (h.primed && time.Since(last) >= livenessSteadyThreshold))) {
				// A queued resume/path change gets a fresh-evidence budget before
				// pre-sleep health can retire this generation. Notification storms
				// cannot extend an already-expired probe budget.
				if !probeExpired && h.nm.consumeNetworkChange() {
					h.requestFreshProbe()
					continue
				}
				if runtime.GOOS == "darwin" {
					h.nm.repairRoutes(ctx)
				}
				return h.primed, errTransportHeartbeatExpired
			}
		}
		select {
		case <-ctx.Done():
			return h.primed, ctx.Err()
		case <-ticker.C:
			if h.nm.consumeNetworkChange() {
				h.requestFreshProbe()
			}
		case <-h.nm.monitorChanges:
			if h.networkDebounce == nil {
				h.networkDebounce = resetHealthTimer(&h.networkTimer, 50*time.Millisecond)
			}
		case <-h.networkDebounce:
			h.networkDebounce = nil
			stopHealthTimer(&h.networkTimer)
			if h.nm.consumeNetworkChange() {
				h.requestFreshProbe()
			}
		case <-h.probeDeadline:
		case <-done[0]:
		case <-done[1]:
		case <-done[2]:
		case id := <-h.nm.checks:
			if id == h.generation.id {
				h.nm.heartbeatStats.RequestProbe()
				if h.primed && h.probeRequested.IsZero() {
					h.probeRequested = time.Now()
					h.probeDeadline = resetHealthTimer(&h.probeTimer, livenessSteadyThreshold)
				}
			}
		case event := <-h.nm.failures:
			if event.generation == h.generation.id {
				return h.primed, event.err
			}
		}
	}
}

func (h *generationHealth) requestFreshProbe() {
	// Repeated notifications cannot extend the recovery budget forever. Only a
	// new path event clears evidence; ordinary ticks never do.
	if !h.armedAt.IsZero() && h.probeRequested.IsZero() {
		h.probeRequested = time.Now()
		h.probeDeadline = resetHealthTimer(&h.probeTimer, livenessSteadyThreshold)
	}
	h.nm.heartbeatStats.ClearPending()
	h.nm.heartbeatStats.RequestProbe()
}

func resetHealthTimer(timer **time.Timer, delay time.Duration) <-chan time.Time {
	if *timer == nil {
		*timer = time.NewTimer(delay)
		return (*timer).C
	}
	current := *timer
	if !current.Stop() {
		select {
		case <-current.C:
		default:
		}
	}
	current.Reset(delay)
	return current.C
}

func stopHealthTimer(timer **time.Timer) {
	if *timer == nil {
		return
	}
	current := *timer
	if !current.Stop() {
		select {
		case <-current.C:
		default:
		}
	}
	*timer = nil
}
