package leases

import (
	"context"
	"net"
)

// Request contains parsed allocation intent, independent of the RPC protocol.
type Request struct {
	OwnerID    string
	ExcludeIPs []net.IP
	ConfirmIP  net.IP
	Hostname   string
}

// Event is an immutable allocation or proposal snapshot delivered to a subscriber.
type Event struct {
	IPv4    string
	IPv6    string
	Version int64
	DryRun  bool
}

// Callbacks connect committed lease changes to the Envoy configuration adapter.
type Callbacks struct {
	Sync   func(context.Context, string, *net.IPNet, *net.IPNet)
	Remove func(context.Context, string)
}

func (s *Manager) run(fn func(context.Context)) {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return
	}
	s.workers.Add(1)
	go func() { defer s.workers.Done(); fn(s.ctx) }()
}

func (s *Manager) syncAsync(owner string, v4, v6 *net.IPNet) {
	s.run(func(ctx context.Context) { s.syncEnvoyRuleIP(ctx, owner, v4, v6) })
}

// Close cancels and joins the reaper and all callbacks owned by this manager.
func (s *Manager) Close() {
	s.workerMu.Lock()
	s.closed = true
	s.cancel()
	s.workerMu.Unlock()
	s.workers.Wait()
}
