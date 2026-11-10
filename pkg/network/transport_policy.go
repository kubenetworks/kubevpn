package network

import (
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	// ipWatcherRetryInterval is the delay before reconnecting the cluster IP watcher.
	ipWatcherRetryInterval = 10 * time.Second
	// watcherDialTimeout bounds readiness only; watch streams retain the session context.
	watcherDialTimeout = 30 * time.Second
	// portForwardPodListTimeout bounds listing the traffic-manager pod before a port-forward session.
	portForwardPodListTimeout = 10 * time.Second
	// portForwardReconnectDelay is the initial/minimum pause between port-forward reconnect attempts.
	portForwardReconnectDelay = 200 * time.Millisecond
	// portForwardReconnectMaxDelay caps the exponential backoff between reconnects, so a
	// sustained apiserver stall is not hammered with rapid Pods.List + TLS-handshake retries.
	// Kept small: once a session is detected dead (now within ~10s, including the black-hole
	// case), the whole tunnel is down until the next session comes up, so a long reconnect gap
	// directly prolongs the outage. 2s still throttles a hard apiserver stall to ~30 retries/min.
	portForwardReconnectMaxDelay = 2 * time.Second
	// portForwardHealthySession is the minimum session duration considered "healthy": a
	// session that lasted at least this long before dropping resets the reconnect backoff
	// (normal pod recreation reconnects fast); shorter sessions grow the backoff. Note this
	// equals livenessStartupDeadline, so duration is NOT sufficient evidence of health — see
	// nextPortForwardDelay, which also requires the session to have carried traffic.
	portForwardHealthySession = 30 * time.Second
	// portForwardBlackHoleMaxDelay caps the backoff once the data plane has repeatedly failed to
	// come up at all. Much larger than portForwardReconnectMaxDelay because reconnecting faster
	// cannot help a black-holed tunnel, while each reconnect also tears down the xDS control
	// stream that shares this session (lease renewal, route discovery, DNS).
	portForwardBlackHoleMaxDelay = 30 * time.Second
	// portForwardBlackHoleThreshold is how many consecutive never-primed sessions constitute a
	// black hole worth reporting to the user rather than retrying quietly.
	portForwardBlackHoleThreshold = 3
	// portForwardStartTimeout is how long to wait for the first port-forward session to become ready.
	portForwardStartTimeout = 60 * time.Second
	// livenessCheckInterval is how often the data-plane liveness watchdog samples HeartbeatStats.
	livenessCheckInterval = 5 * time.Second
	// livenessStartupDeadline bounds how long a session may run without ever producing a fresh
	// heartbeat echo reply (never becoming ready, or ready-but-black-holed from the start) before it
	// is force-reconnected. Must be >> the time for a healthy reconnected session to prime a reply
	// (slot reconnect <= SlotReconnectBackoff + RTT), so a healthy session is never torn down for a
	// reply it has not had time to receive.
	livenessStartupDeadline = 30 * time.Second
	// rentIPCallTimeout bounds a single GetTunIP call so a black-holed port-forward during startup
	// (which no longer tears itself down via the xDS health check) makes the call time out and retry
	// instead of hanging forever. The watchdog reconnects the session underneath; a later attempt
	// then lands on a healthy session.
	rentIPCallTimeout = 10 * time.Second
)

// blackHoleWarn rate-limits the "data plane never came up" escalation so a tunnel that stays down
// for hours reports periodically rather than once (invisible to anyone reading recent logs) or on
// every attempt (a flood).
var blackHoleWarn = plog.NewThrottle(5 * time.Minute)

// livenessSteadyThreshold is how long an already-primed session (one that produced a fresh heartbeat
// echo reply) may then go silent before it is treated as black-holed and force-reconnected. A small
// multiple of config.HeartbeatInterval so a couple of missed beats trip it. It is a var (not a const)
// because config.HeartbeatInterval is a runtime var.
var livenessSteadyThreshold = 3 * config.HeartbeatInterval

// nextPortForwardDelay computes the next reconnect backoff. Only a session that BOTH carried
// data-plane traffic (primed) and lasted at least portForwardHealthySession counts as healthy and
// resets to the fast initial delay; anything else doubles the delay.
//
// Duration alone is not evidence of health, and treating it as such disabled the backoff entirely
// for the case that needed it most: the liveness watchdog tears a black-holed session down after
// livenessStartupDeadline, which is exactly portForwardHealthySession, so every black-holed session
// looked "healthy" and reset the delay to 200ms. The result was a Pods.List + SPDY handshake every
// 30s for 20 hours, with no convergence and no escalation.
//
// A never-primed session backs off to the larger portForwardBlackHoleMaxDelay: the tunnel is down
// regardless of how fast we retry, so a longer gap costs nothing and buys the control plane (xDS
// lease renewal, route discovery, DNS — all sharing this one port-forward session) uninterrupted
// time between attempts. Pure function for testability.
func nextPortForwardDelay(cur, sessionDuration time.Duration, primed bool) time.Duration {
	if primed && sessionDuration >= portForwardHealthySession {
		return portForwardReconnectDelay
	}
	maxDelay := portForwardReconnectMaxDelay
	if !primed {
		maxDelay = portForwardBlackHoleMaxDelay
	}
	next := cur * 2
	if next < portForwardReconnectDelay {
		next = portForwardReconnectDelay
	}
	if next > maxDelay {
		next = maxDelay
	}
	return next
}

// transportRetryPolicy is the concrete retry state for one supervisor. It
// makes no I/O or publication decisions; the serial supervisor executes them.
type transportRetryPolicy struct {
	kinds           []transportKind
	index           int
	failed          int
	delay           time.Duration
	consecutiveDead int
}

func newTransportRetryPolicy(cfg *Config) *transportRetryPolicy {
	kinds := []transportKind{transportPF}
	if cfg.SshConf != nil && !cfg.SshConf.IsEmpty() {
		kinds = []transportKind{transportSSH, transportPF}
	}
	return &transportRetryPolicy{kinds: kinds, delay: portForwardReconnectDelay}
}

func (p *transportRetryPolicy) currentKind() transportKind {
	return p.kinds[p.index]
}

func (p *transportRetryPolicy) recordSession(sessionDuration time.Duration, primed bool) {
	p.delay = nextPortForwardDelay(p.delay, sessionDuration, primed)
	if primed {
		p.consecutiveDead = 0
		p.failed = 0
	} else {
		p.consecutiveDead++
	}
}

// advanceAfterFailure selects the next kind and reports whether every
// configured path has now failed and a bounded wait is required.
func (p *transportRetryPolicy) advanceAfterFailure() bool {
	p.failed++
	p.index = (p.index + 1) % len(p.kinds)
	return p.failed >= len(p.kinds)
}

func (p *transportRetryPolicy) finishRound(hadSession bool) {
	if !hadSession {
		p.delay = nextPortForwardDelay(p.delay, 0, true)
	}
	p.failed = 0
}
