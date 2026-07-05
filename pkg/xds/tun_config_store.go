package xds

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// loadAllocs reads the persisted ownerID→IP mapping from the ConfigMap.
func (s *TunConfigServer) loadAllocs(ctx context.Context) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil || cm.Data == nil {
		return
	}
	persisted, err := parsePersistedAllocs(cm.Data[config.KeyTunAllocs])
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] Failed to load persisted allocs: %v", err)
		return
	}
	if len(persisted) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	loaded, released := 0, 0
	for ownerID, pa := range persisted {
		lastRenew := time.Unix(pa.LastRenew, 0)
		v4IP, v4Net, _ := net.ParseCIDR(pa.IPv4)
		v6IP, v6Net, _ := net.ParseCIDR(pa.IPv6)
		if v4Net != nil {
			v4Net.IP = v4IP
		}
		if v6Net != nil {
			v6Net.IP = v6IP
		}
		if v4Net == nil {
			continue
		}

		if now.Sub(lastRenew) > LeaseDuration {
			var ipv6 net.IP
			if v6Net != nil {
				ipv6 = v6Net.IP
			}
			if err := s.dhcp.ReleaseIP(ctx, v4Net.IP, ipv6); err != nil {
				plog.G(ctx).Warnf("[TunConfig] Failed to release expired IP %v for %s: %v", v4Net, ownerID, err)
			}
			released++
			plog.G(ctx).Debugf("[TunConfig] Expired alloc for %s (last renew %v), released %v", ownerID, lastRenew, v4Net)
			continue
		}

		s.allocs[ownerID] = &tunAllocation{
			IPv4:      v4Net,
			IPv6:      v6Net,
			Version:   pa.Version,
			LastRenew: lastRenew,
			Hostname:  pa.Hostname,
		}
		loaded++
	}
	if loaded > 0 || released > 0 {
		plog.G(ctx).Infof("[TunConfig] Restored %d IP allocations, released %d expired from ConfigMap", loaded, released)
	}
	if released > 0 {
		if err := s.saveAllocs(ctx); err != nil {
			plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs after cleanup: %v", err)
		}
	}
}

// parsePersistedAllocs decodes the TUN_ALLOCS YAML (ownerID → persistedAlloc).
// An empty string yields an empty map and no error.
func parsePersistedAllocs(data string) (map[string]*persistedAlloc, error) {
	if data == "" {
		return map[string]*persistedAlloc{}, nil
	}
	var persisted map[string]*persistedAlloc
	if err := yaml.Unmarshal([]byte(data), &persisted); err != nil {
		return nil, err
	}
	return persisted, nil
}

// saveAllocs persists the current allocs map to ConfigMap.
// Caller must hold s.mu (read or write lock) or ensure no concurrent modification.
func (s *TunConfigServer) saveAllocs(ctx context.Context) error {
	persisted := make(map[string]*persistedAlloc, len(s.allocs))
	for ownerID, alloc := range s.allocs {
		pa := &persistedAlloc{
			Version:   alloc.Version,
			LastRenew: alloc.LastRenew.Unix(),
			Hostname:  alloc.Hostname,
		}
		if alloc.IPv4 != nil {
			pa.IPv4 = alloc.IPv4.String()
		}
		if alloc.IPv6 != nil {
			pa.IPv6 = alloc.IPv6.String()
		}
		persisted[ownerID] = pa
	}

	data, err := yaml.Marshal(persisted)
	if err != nil {
		return err
	}

	// Authoritative, optimistic-locked write of this server's in-memory lease
	// map. The in-memory map is the source of truth (it already reflects reaps
	// and renews), so TUN_ALLOCS is overwritten rather than merged — merging
	// would resurrect intentionally-reclaimed leases. RetryOnConflict re-reads
	// on a conflicting concurrent write (e.g. a bitmap or envoy patch bumping
	// the ResourceVersion), so other ConfigMap keys are never clobbered.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, getErr := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyTunAllocs] = string(data)
		// Fold the rejection breadcrumb into this same Update so it shares the
		// allocation write's optimistic lock — a single writer, never clobbered.
		if s.rejectedAnno != "" {
			if cm.Annotations == nil {
				cm.Annotations = make(map[string]string)
			}
			cm.Annotations[annoTunAllocsRejected] = s.rejectedAnno
		}
		_, updErr := s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return updErr
	})
}

// recordRejectedLocked stages a breadcrumb explaining why a manual TUN_ALLOCS edit
// was not applied (client conflict, IP in use, or expiry) so an operator can see it
// via `kubectl describe cm`. The message is persisted by the next saveAllocs (the
// single ConfigMap writer), which avoids racing a separate annotation patch against
// the allocation write. A nil old keeps the message short. Caller holds s.mu.
func (s *TunConfigServer) recordRejectedLocked(owner string, rejected, old net.IP) {
	if old != nil {
		s.rejectedAnno = fmt.Sprintf("%s: requested IP %s rejected by client (local conflict), kept %s", owner, rejected, old)
	} else {
		s.rejectedAnno = fmt.Sprintf("%s: requested IP %s not applied (in use or unavailable)", owner, rejected)
	}
}

// persistRejected flushes a staged rejection breadcrumb to the ConfigMap promptly,
// for call sites that record one without an allocation change of their own (so no
// saveAllocs would otherwise follow). saveAllocs reads only s.allocs/s.rejectedAnno,
// so a read lock is enough.
func (s *TunConfigServer) persistRejected(ctx context.Context) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.saveAllocs(ctx); err != nil {
		plog.G(ctx).Warnf("[TunConfig] persist rejection annotation: %v", err)
	}
}

// ReconcileDHCP is called when the ConfigMap DHCP data changes.
// It checks all registered allocations and notifies owners whose IPs are no longer valid.
func (s *TunConfigServer) ReconcileDHCP(ctx context.Context) {
	s.mu.RLock()
	owners := make(map[string]*tunAllocation, len(s.allocs))
	for k, v := range s.allocs {
		owners[k] = v
	}
	s.mu.RUnlock()

	allocatedIPs := make(map[string]bool)
	_ = s.dhcp.ForEach(ctx, func(ip net.IP) {
		allocatedIPs[ip.String()] = true
	}, func(ip net.IP) {
		allocatedIPs[ip.String()] = true
	})

	changed := false
	for ownerID, alloc := range owners {
		if alloc.IPv4 != nil && !allocatedIPs[alloc.IPv4.IP.String()] {
			plog.G(ctx).Warnf("[TunConfig] IP %s for owner %s lost, re-allocating", alloc.IPv4, ownerID)
			newV4, newV6, err := s.dhcp.RentIP(ctx)
			if err != nil {
				plog.G(ctx).Errorf("[TunConfig] Failed to re-rent for %s: %v", ownerID, err)
				continue
			}
			s.NotifyIPChange(ownerID, newV4, newV6)
			changed = true
		}
	}
	if changed {
		s.mu.RLock()
		err := s.saveAllocs(ctx)
		s.mu.RUnlock()
		if err != nil {
			plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs after reconciliation: %v", err)
		}
	}
}

// ReconcileAllocsFromConfigMap honors manual operator edits to the TUN_ALLOCS
// ConfigMap key. Normally TUN_ALLOCS is a server-written journal; this makes an
// external edit a control input: if an owner's IPv4 there differs from the
// server's in-memory allocation, the edited IP becomes the desired address and
// is applied end-to-end — the bitmap (TUN_IP_POOL) is updated, any other owner
// holding that IP is forced off it (re-rented + pushed), and the owning client
// is pushed the change via WatchTunIP so its local TUN device address updates in
// place (ChangeTunIP). Wired as an informer onDHCPChange callback.
//
// Idempotent: an owner whose desired IP already equals its current IP is skipped,
// so the server's own saveAllocs writes do not loop back through here.
func (s *TunConfigServer) ReconcileAllocsFromConfigMap(ctx context.Context) {
	// Serialize reconciles so two overlapping informer events never act on each
	// other's stale snapshot.
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	// Re-read/merge loop: each pass re-reads the latest TUN_ALLOCS and applies any
	// remaining diff, so an operator edit landing during a reconcile is applied on
	// the next pass instead of being silently overwritten by saveAllocs. Converges
	// when a pass changes nothing; an edit landing after the last pass triggers its
	// own informer event.
	for round := 0; round < maxReconcileRounds; round++ {
		if !s.reconcileManualOnce(ctx) {
			return
		}
	}
	plog.G(ctx).Warnf("[TunConfig] reconcile hit maxReconcileRounds=%d; will converge on next informer event", maxReconcileRounds)
}

// reconcileManualOnce performs one fresh-read reconcile pass over TUN_ALLOCS and
// reports whether it changed any allocation. Caller holds reconcileMu.
func (s *TunConfigServer) reconcileManualOnce(ctx context.Context) bool {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Errorf("[TunConfig] reconcile: get configmap: %v", err)
		return false
	}
	desired, err := parsePersistedAllocs(cm.Data[config.KeyTunAllocs])
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] reconcile: cannot parse %s: %v", config.KeyTunAllocs, err)
		return false
	}

	// Snapshot committed allocations (used to detect "target IP in use by another
	// owner"). proposeIPChange does its own locking, so we don't hold s.mu here.
	s.mu.RLock()
	current := make(map[string]*tunAllocation, len(s.allocs))
	for k, v := range s.allocs {
		cp := *v
		current[k] = &cp
	}
	s.mu.RUnlock()

	// Deterministic order for stable logging when multiple owners are edited.
	owners := make([]string, 0, len(desired))
	for o := range desired {
		owners = append(owners, o)
	}
	sort.Strings(owners)

	proposed := false
	for _, owner := range owners {
		if s.proposeManualChange(ctx, owner, desired[owner], current) {
			proposed = true
		}
	}
	// Note: proposing does NOT touch committed state (allocs/bitmap/TUN_ALLOCS), so
	// there is nothing to saveAllocs here. The operator's edit stays in TUN_ALLOCS
	// as the desired value until the client confirms (commit) or declines (revert).
	return proposed
}

// proposeManualChange offers (dry-run) one owner's desired IPv4 and/or IPv6 from
// TUN_ALLOCS to the client, per family. It does NOT commit: no IP is rented, no
// allocation changes. The client validates each family locally and confirms (→
// GetTunIP commits) or declines. Returns whether a NEW proposal was pushed.
// Caller holds reconcileMu (not s.mu).
func (s *TunConfigServer) proposeManualChange(ctx context.Context, owner string, want *persistedAlloc, current map[string]*tunAllocation) bool {
	s.mu.RLock()
	live, ok := s.allocs[owner]
	var curV4, curV6 *net.IPNet
	if ok {
		curV4, curV6 = live.IPv4, live.IPv6
	}
	pend := s.pendingProposal[owner] // zero value (nil slots) if absent
	liveVersion := int64(0)
	if ok {
		liveVersion = live.Version
	}
	s.mu.RUnlock()
	if !ok {
		return false // owner not active in memory (offline / reaped) — nothing to propose
	}

	// Stale-echo guard. TUN_ALLOCS is both the operator's input and the server's journal
	// output (saveAllocs). The root cause is a TOCTOU, NOT apiserver cache staleness:
	// reconcileManualOnce reads the ConfigMap (its `Get` is already an etcd-consistent
	// quorum read) BEFORE taking s.mu, but reads s.allocs AFTER — while a concurrent
	// commit holds s.mu across its saveAllocs. So `want` (desired, read from the CM) can
	// be the value from BEFORE that commit's saveAllocs while the snapshot used as
	// "current" is from AFTER it: desired != current → re-propose the previous committed
	// value forever (the ::1<->::3 oscillation). Making the read "more consistent" does
	// NOT help (it is already consistent); the read of the CM and the read of memory
	// straddle a commit. The version is monotonic (bumped on every commit/decline/expire),
	// so a CM whose version is strictly older than the in-memory allocation is exactly
	// such a cross-commit stale read — skip it. A real edit (`kubectl edit`, which keeps
	// the version field, or one that omits it → 0) carries version == current (or 0).
	if want.Version != 0 && want.Version < liveVersion {
		return false
	}

	candV4 := s.familyCandidate(ctx, owner, want.IPv4, curV4, pend.candV4, current, false)
	candV6 := s.familyCandidate(ctx, owner, want.IPv6, curV6, pend.candV6, current, true)
	if candV4 == nil && candV6 == nil {
		return false
	}
	s.proposeIPChange(owner, candV4, candV6)
	plog.G(ctx).Infof("[TunConfig] proposed manual TUN IP v4=%v v6=%v to owner %s (dry-run; awaiting client confirm)", candV4, candV6, owner)
	return true
}

// proposeIPChange merges the given per-family candidates into the owner's pending
// proposal and pushes a DryRun TunIPResponse carrying ONLY the (newly) proposed
// families. No IP is rented; commit happens only at GetTunIP{ConfirmIP}.
func (s *TunConfigServer) proposeIPChange(owner string, candV4, candV6 *net.IPNet) {
	if candV4 == nil && candV6 == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pendingProposal[owner]
	if candV4 != nil {
		p.candV4 = candV4
	}
	if candV6 != nil {
		p.candV6 = candV6
	}
	p.deadline = time.Now().Add(manualGrace)
	s.pendingProposal[owner] = p
	resp := &rpc.TunIPResponse{Version: time.Now().UnixNano(), DryRun: true}
	if candV4 != nil {
		resp.IPv4 = candV4.String()
	}
	if candV6 != nil {
		resp.IPv6 = candV6.String()
	}
	s.notifyWatchers(owner, resp)
}

// scrubOrphanBits releases bitmap bits not owned by any live allocation. Orphans
// arise if the process died between renting an IP (TUN_IP_POOL) and persisting
// the owner record (TUN_ALLOCS) — the two are separate ConfigMap keys. Without
// this, such bits are never renewed or reaped and slowly exhaust the pool.
func (s *TunConfigServer) scrubOrphanBits(ctx context.Context) {
	s.mu.Lock()
	owned := make(map[string]bool, len(s.allocs)*2)
	for _, alloc := range s.allocs {
		if alloc.IPv4 != nil {
			owned[alloc.IPv4.IP.String()] = true
		}
		if alloc.IPv6 != nil {
			owned[alloc.IPv6.IP.String()] = true
		}
	}
	// Note: dry-run proposals reserve no IP (commit rents only on confirm), so scrub
	// needs no special-casing for them.
	var orphans []net.IP
	collect := func(ip net.IP) {
		if !owned[ip.String()] {
			orphans = append(orphans, ip)
		}
	}
	err := s.dhcp.ForEach(ctx, collect, collect)
	s.mu.Unlock()
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] scrub: failed to enumerate bitmap: %v", err)
		return
	}

	// An orphan IP's bit is set, so a concurrent GetTunIP (AllocateNext returns
	// only unset bits) cannot claim it between here and the release.
	if len(orphans) == 0 {
		return
	}
	if err := s.dhcp.ReleaseIPs(ctx, orphans...); err != nil {
		plog.G(ctx).Warnf("[TunConfig] scrub: failed to release %d orphan bits: %v", len(orphans), err)
		return
	}
	plog.G(ctx).Infof("[TunConfig] scrub reclaimed %d orphan bitmap bits", len(orphans))
}
