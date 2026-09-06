package xds

import (
	"context"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// WatcherCount returns how many WatchTunIP streams are currently subscribed for an
// owner. Used for observability and to let tests wait until a client has subscribed
// before pushing (a dry-run proposal pushed to no watcher would be lost).
func (s *TunConfigServer) WatcherCount(ownerID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.watchers[ownerID])
}

// notifyWatchers pushes a TunIPResponse to all WatchTunIP subscribers for the given ownerID.
// Must be called with s.mu held.
func (s *TunConfigServer) notifyWatchers(ownerID string, resp *rpc.TunIPResponse) {
	for _, ch := range s.watchers[ownerID] {
		select {
		case ch <- resp:
		default:
		}
	}
}

// WatchTunIP streams IP changes to the caller. Blocks until context is cancelled.
// An active stream acts as an implicit lease renewal — LastRenew is refreshed
// periodically so the LeaseReaper won't reclaim the IP while the stream is alive.
func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream rpc.TunConfigService_WatchTunIPServer) error {
	ch := make(chan *rpc.TunIPResponse, 4)

	s.mu.Lock()
	s.watchers[req.OwnerID] = append(s.watchers[req.OwnerID], ch)
	s.renewLease(req.OwnerID)
	// Replay any outstanding dry-run proposal to this (re)subscriber as an initial
	// snapshot. A proposal pushed via notifyWatchers while the client was briefly
	// absent — e.g. mid-reconnect after a xds outage, when a stale watcher
	// channel still lingered — reaches no live client and is otherwise lost: the
	// server keeps the pending intent but never re-pushes it, so the client never
	// confirms and the operator's edit silently stalls. Re-delivering here closes
	// that window. Buffered send mirrors notifyWatchers (the channel is fresh/empty).
	if snap := s.pendingProposalResp(req.OwnerID); snap != nil {
		select {
		case ch <- snap:
		default:
		}
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.removeWatcher(req.OwnerID, ch)
		s.mu.Unlock()
	}()

	ticker := time.NewTicker(LeaseDuration / 3)
	defer ticker.Stop()

	for {
		select {
		case resp := <-ch:
			if err := stream.Send(resp); err != nil {
				return err
			}
		case <-ticker.C:
			s.mu.Lock()
			s.renewLease(req.OwnerID)
			if alloc, ok := s.allocs[req.OwnerID]; ok {
				resp := &rpc.TunIPResponse{Version: alloc.Version}
				if alloc.IPv4 != nil {
					resp.IPv4 = alloc.IPv4.String()
				}
				if alloc.IPv6 != nil {
					resp.IPv6 = alloc.IPv6.String()
				}
				select {
				case ch <- resp:
				default:
				}
			}
			if err := s.saveAllocs(stream.Context()); err != nil {
				plog.G(stream.Context()).Warnf("[TunConfig] Failed to persist lease renewal for %s: %v", req.OwnerID, err)
			}
			s.mu.Unlock()
		case <-stream.Context().Done():
			return nil
		}
	}
}

// NotifyIPChange is called by the ConfigMap watcher when an owner's IP changes.
// It updates the allocation, pushes to all watchers, and syncs the envoy rule IP
// so mesh traffic for this owner follows the new TUN IP (the GetTunIP path syncs
// directly; this covers reconcile-driven changes).
func (s *TunConfigServer) NotifyIPChange(ownerID string, newIPv4, newIPv6 *net.IPNet) {
	s.mu.Lock()
	alloc, ok := s.allocs[ownerID]
	if !ok {
		alloc = &tunAllocation{}
		s.allocs[ownerID] = alloc
	}
	alloc.IPv4 = newIPv4
	alloc.IPv6 = newIPv6
	alloc.Version = time.Now().UnixNano()

	resp := &rpc.TunIPResponse{Version: alloc.Version}
	if newIPv4 != nil {
		resp.IPv4 = newIPv4.String()
	}
	if newIPv6 != nil {
		resp.IPv6 = newIPv6.String()
	}

	s.notifyWatchers(ownerID, resp)
	s.mu.Unlock()

	go s.syncEnvoyRuleIP(context.Background(), ownerID, newIPv4, newIPv6)
}

// StartLeaseReaper launches a background goroutine that periodically checks for
// expired allocations and reclaims them.
func (s *TunConfigServer) StartLeaseReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(leaseReapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.reapExpiredLeases(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *TunConfigServer) reapExpiredLeases(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	var expired []string
	for ownerID, alloc := range s.allocs {
		if now.Sub(alloc.LastRenew) > LeaseDuration {
			expired = append(expired, ownerID)
		}
	}
	s.mu.Unlock()

	for _, ownerID := range expired {
		s.mu.Lock()
		alloc, ok := s.allocs[ownerID]
		if !ok || time.Since(alloc.LastRenew) <= LeaseDuration {
			s.mu.Unlock()
			continue
		}
		delete(s.allocs, ownerID)
		// Remember the IP so this owner reclaims it on reconnect if still free.
		s.lastIPs[ownerID] = lastIPRecord{v4: alloc.IPv4, v6: alloc.IPv6}
		// Mark when the lease was reaped (only if not already marked, so the abandonment
		// clock starts at the first reap and is not reset by re-reaps). Cleared on GetTunIP.
		if _, marked := s.reapedAt[ownerID]; !marked {
			s.reapedAt[ownerID] = now
		}
		s.mu.Unlock()

		var ipv4, ipv6 net.IP
		if alloc.IPv4 != nil {
			ipv4 = alloc.IPv4.IP
		}
		if alloc.IPv6 != nil {
			ipv6 = alloc.IPv6.IP
		}
		if err := s.dhcp.ReleaseIP(ctx, ipv4, ipv6); err != nil {
			plog.G(ctx).Warnf("[TunConfig] Failed to release IP %v for expired owner %s: %v", alloc.IPv4, ownerID, err)
		}
		plog.G(ctx).Infof("[TunConfig] Lease expired for owner %s, reclaimed IP %v", ownerID, alloc.IPv4)
	}

	if len(expired) > 0 {
		s.mu.RLock()
		err := s.saveAllocs(ctx)
		s.mu.RUnlock()
		if err != nil {
			plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs after lease reap: %v", err)
		}
	}

	// Abandonment pass: an owner whose lease was reaped and has not re-acquired within
	// abandonmentTTL (and has no live watcher/alloc) is treated as gone for good (crash /
	// SIGKILL / powered off) — clean up its leftover envoy rules so a stale proxy rule does
	// not linger indefinitely. A sleeping client is unaffected: it re-acquires via GetTunIP
	// on wake (which clears reapedAt) well within the (long) TTL.
	s.reapAbandonedOwners(ctx, now)

	// Drop dry-run proposals whose grace deadline elapsed with no client response
	// (offline/lost push). No IP was rented for a proposal, so there is nothing to
	// release — just revert any operator edit still sitting in TUN_ALLOCS to actual.
	s.expirePendingProposals(ctx)

	// Reclaim bitmap bits that no live allocation owns (e.g. leaked by a crash
	// between renting and persisting). Safe under the single-writer (Recreate)
	// deployment: GetTunIP is mutex-serialized, so no in-flight allocation is
	// misclassified as an orphan.
	s.scrubOrphanBits(ctx)
}

// reapAbandonedOwners removes the envoy rules of owners whose lease was reaped more than
// abandonmentTTL ago and which have not re-acquired (no alloc, no watcher). Candidates are
// collected under the lock; the ConfigMap read-modify-write runs outside it.
func (s *TunConfigServer) reapAbandonedOwners(ctx context.Context, now time.Time) {
	var abandoned []string
	s.mu.Lock()
	for owner, t := range s.reapedAt {
		if now.Sub(t) <= abandonmentTTL {
			continue
		}
		if _, hasAlloc := s.allocs[owner]; hasAlloc {
			delete(s.reapedAt, owner) // re-acquired; no longer abandoned
			continue
		}
		if len(s.watchers[owner]) > 0 {
			continue // still connected via a stream; not abandoned
		}
		abandoned = append(abandoned, owner)
		delete(s.reapedAt, owner)
		delete(s.lastIPs, owner)
	}
	s.mu.Unlock()

	for _, owner := range abandoned {
		s.removeEnvoyRulesForOwner(ctx, owner)
	}
}

// expirePendingProposals drops dry-run proposals past their deadline. Proposals hold
// no rented IP, so expiry just clears the intent, bumps the owner's version, and
// overwrites the operator's unconfirmed edit in TUN_ALLOCS with the committed actual.
// Bumping makes the expired edit strictly older, so a lagging read of it is rejected by
// the stale-echo guard and is not re-proposed.
func (s *TunConfigServer) expirePendingProposals(ctx context.Context) {
	now := time.Now()
	expired := false
	s.mu.Lock()
	for owner, p := range s.pendingProposal {
		if now.After(p.deadline) {
			delete(s.pendingProposal, owner)
			if a, ok := s.allocs[owner]; ok {
				a.Version = time.Now().UnixNano()
			}
			expired = true
			plog.G(ctx).Debugf("[TunConfig] proposal for owner %s expired (no client response); dropping", owner)
		}
	}
	if expired {
		if err := s.saveAllocs(ctx); err != nil {
			plog.G(ctx).Warnf("[TunConfig] revert TUN_ALLOCS after proposal expiry: %v", err)
		}
	}
	s.mu.Unlock()
}

// renewLease refreshes LastRenew for the given ownerID. Caller must hold s.mu.
func (s *TunConfigServer) renewLease(ownerID string) {
	if alloc, ok := s.allocs[ownerID]; ok {
		alloc.LastRenew = time.Now()
	}
}

func (s *TunConfigServer) removeWatcher(ownerID string, ch chan *rpc.TunIPResponse) {
	watchers := s.watchers[ownerID]
	for i, w := range watchers {
		if w == ch {
			s.watchers[ownerID] = append(watchers[:i], watchers[i+1:]...)
			break
		}
	}
	if len(s.watchers[ownerID]) == 0 {
		delete(s.watchers, ownerID)
	}
	close(ch)
}
