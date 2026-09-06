package xds

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// GetTunIP allocates or retrieves the TUN IP for the given owner.
// When ExcludeIPs is set and the existing allocation conflicts with one of them,
// the old IP is released and a new one is allocated (skipping all ExcludeIPs).
func (s *TunConfigServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	excludeIPs := parseExcludeIPs(req.ExcludeIPs)

	s.mu.Lock()
	defer s.mu.Unlock()

	// The owner is active (or returning from sleep): it is no longer abandoned, so it
	// keeps its envoy rules regardless of how long its lease had lapsed.
	delete(s.reapedAt, req.OwnerID)

	// Dry-run confirm: the client validated a proposed IP (v4 or v6) and asks to
	// commit it. The family is inferred from the ConfirmIP address.
	if req.ConfirmIP != "" {
		if p, ok := s.pendingProposal[req.OwnerID]; ok {
			cip := net.ParseIP(req.ConfirmIP)
			isV6 := cip != nil && cip.To4() == nil
			cand := p.candV4
			if isV6 {
				cand = p.candV6
			}
			if cand != nil && cand.IP.String() == req.ConfirmIP {
				return s.commitFamilyLocked(ctx, req.OwnerID, isV6, cand, excludeIPs, req.Hostname)
			}
		}
		// stale/unknown confirm → fall through to normal handling
	}

	// Dry-run decline: the client rejected a pending proposal family (its candidate
	// is in ExcludeIPs). Drop that family's slot and keep the current committed IP —
	// nothing was committed, so there is no rollback. Return current explicitly (do
	// NOT realloc even if the client also excluded its own current IP).
	if p, ok := s.pendingProposal[req.OwnerID]; ok &&
		((p.candV4 != nil && isIPExcluded(p.candV4, excludeIPs)) || (p.candV6 != nil && isIPExcluded(p.candV6, excludeIPs))) {
		var curV4 net.IP
		if a, aok := s.allocs[req.OwnerID]; aok && a.IPv4 != nil {
			curV4 = a.IPv4.IP
		}
		if p.candV4 != nil && isIPExcluded(p.candV4, excludeIPs) {
			plog.G(ctx).Warnf("[TunConfig] owner %s declined proposed IPv4 %v (local conflict)", req.OwnerID, p.candV4.IP)
			s.recordRejectedLocked(req.OwnerID, p.candV4.IP, curV4)
			p.candV4 = nil
		}
		if p.candV6 != nil && isIPExcluded(p.candV6, excludeIPs) {
			plog.G(ctx).Warnf("[TunConfig] owner %s declined proposed IPv6 %v (local conflict)", req.OwnerID, p.candV6.IP)
			s.recordRejectedLocked(req.OwnerID, p.candV6.IP, curV4)
			p.candV6 = nil
		}
		if p.candV4 == nil && p.candV6 == nil {
			delete(s.pendingProposal, req.OwnerID)
		} else {
			s.pendingProposal[req.OwnerID] = p
		}
		// Bump the version and overwrite the operator's declined edit in TUN_ALLOCS with
		// the committed actual. Bumping makes the declined edit (which carried the old
		// version) strictly older, so a lagging read of it is rejected by the stale-echo
		// guard and the decline is not re-proposed. saveAllocs also folds in the rejection
		// breadcrumb (s.rejectedAnno).
		if a, aok := s.allocs[req.OwnerID]; aok {
			a.Version = time.Now().UnixNano()
		}
		if err := s.saveAllocs(ctx); err != nil {
			plog.G(ctx).Warnf("[TunConfig] persist rejection after decline: %v", err)
		}
		if a, aok := s.allocs[req.OwnerID]; aok {
			a.LastRenew = time.Now()
			return tunResp(a), nil
		}
		// no committed alloc (rare) → fall through to fresh allocation
	}

	if alloc, ok := s.allocs[req.OwnerID]; ok {
		if !isIPExcluded(alloc.IPv4, excludeIPs) {
			alloc.LastRenew = time.Now()
			if req.Hostname != "" {
				alloc.Hostname = req.Hostname
			}
			return tunResp(alloc), nil
		}
		// Committed IP conflicts with client ExcludeIPs → re-allocate a safe IP.
		delete(s.allocs, req.OwnerID)
		plog.G(ctx).Infof("[TunConfig] IP %v conflicts with client ExcludeIPs, re-allocating for owner %s", alloc.IPv4, req.OwnerID)
		v4, v6, err := s.dhcp.RentIPExcluding(ctx, excludeIPs)
		if err != nil {
			return nil, err
		}
		var oldV4, oldV6 net.IP
		if alloc.IPv4 != nil {
			oldV4 = alloc.IPv4.IP
		}
		if alloc.IPv6 != nil {
			oldV6 = alloc.IPv6.IP
		}
		if err := s.dhcp.ReleaseIP(ctx, oldV4, oldV6); err != nil {
			plog.G(ctx).Warnf("[TunConfig] Failed to release old IP %v: %v", oldV4, err)
		}
		newAlloc := &tunAllocation{IPv4: v4, IPv6: v6, Version: time.Now().UnixNano(), LastRenew: time.Now(), Hostname: req.Hostname}
		s.allocs[req.OwnerID] = newAlloc
		plog.G(ctx).Infof("[TunConfig] Re-allocated %s/%s for owner %s", v4, v6, req.OwnerID)
		if err := s.saveAllocs(ctx); err != nil {
			plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs: %v", err)
			s.rollbackAlloc(ctx, req.OwnerID, v4, v6)
			return nil, err
		}
		resp := tunResp(newAlloc)
		s.notifyWatchers(req.OwnerID, resp)
		go s.syncEnvoyRuleIP(context.Background(), req.OwnerID, v4, v6)
		return resp, nil
	}

	// New allocation — hold mutex across RentIP (fast ConfigMap operation, no need to release).
	// Prefer the IP this owner most recently held (sticky across lease expiry),
	// unless it now conflicts with the client's local/sibling IPs (excludeIPs).
	v4, v6, err := s.allocateForOwner(ctx, req.OwnerID, excludeIPs)
	if err != nil {
		return nil, err
	}

	alloc := &tunAllocation{
		IPv4:      v4,
		IPv6:      v6,
		Version:   time.Now().UnixNano(),
		LastRenew: time.Now(),
		Hostname:  req.Hostname,
	}
	s.allocs[req.OwnerID] = alloc

	plog.G(ctx).Infof("[TunConfig] Allocated %s/%s for owner %s", v4, v6, req.OwnerID)
	if err := s.saveAllocs(ctx); err != nil {
		plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs, rolling back: %v", err)
		s.rollbackAlloc(ctx, req.OwnerID, v4, v6)
		return nil, err
	}

	resp := &rpc.TunIPResponse{IPv4: v4.String(), IPv6: v6.String(), Version: alloc.Version}
	s.notifyWatchers(req.OwnerID, resp)
	go s.syncEnvoyRuleIP(context.Background(), req.OwnerID, v4, v6)
	return resp, nil
}

// tunResp builds a committed (DryRun=false) TunIPResponse from an allocation.
func tunResp(alloc *tunAllocation) *rpc.TunIPResponse {
	resp := &rpc.TunIPResponse{Version: alloc.Version}
	if alloc.IPv4 != nil {
		resp.IPv4 = alloc.IPv4.String()
	}
	if alloc.IPv6 != nil {
		resp.IPv6 = alloc.IPv6.String()
	}
	return resp
}

// commitFamilyLocked is the single commit point for a confirmed dry-run proposal of
// ONE address family (v4 or v6). It rents the candidate (falling back to a safe IP
// if it was taken since the proposal), releases the owner's old IP of that family,
// leaves the other family untouched, persists, and pushes the committed (DryRun=
// false) full allocation. Caller holds s.mu.
func (s *TunConfigServer) commitFamilyLocked(ctx context.Context, owner string, isV6 bool, candidate *net.IPNet, excludeIPs []net.IP, hostname string) (*rpc.TunIPResponse, error) {
	cur := s.allocs[owner]
	var curV4, curV6 *net.IPNet
	if cur != nil {
		curV4, curV6 = cur.IPv4, cur.IPv6
	}

	// Refuse if another live owner holds the candidate (same family; no silent takeover).
	for o, a := range s.allocs {
		if o == owner {
			continue
		}
		held := a.IPv4
		if isV6 {
			held = a.IPv6
		}
		if held != nil && held.IP.Equal(candidate.IP) {
			plog.G(ctx).Warnf("[TunConfig] commit %v for owner %s: in use by owner %s, refusing", candidate.IP, owner, o)
			s.recordRejectedLocked(owner, candidate.IP, nil)
			go s.persistRejected(context.Background())
			s.clearProposalFamily(owner, isV6)
			if cur != nil {
				return tunResp(cur), nil
			}
			return nil, fmt.Errorf("candidate %v in use", candidate.IP)
		}
	}

	// Rent the candidate for this family; fall back to a safe IP if it was taken.
	committed := candidate
	var rentErr error
	if isV6 {
		rentErr = s.dhcp.RentSpecificIP(ctx, nil, candidate.IP)
	} else {
		rentErr = s.dhcp.RentSpecificIP(ctx, candidate.IP, nil)
	}
	if rentErr != nil {
		plog.G(ctx).Warnf("[TunConfig] commit %v for owner %s failed (%v); allocating a safe IP", candidate.IP, owner, rentErr)
		v4, v6, ferr := s.dhcp.RentIPExcluding(ctx, excludeIPs)
		if ferr != nil {
			return nil, ferr
		}
		if isV6 {
			committed = v6
			_ = s.dhcp.ReleaseIPs(ctx, v4.IP) // release the unused paired v4
		} else {
			committed = v4
			_ = s.dhcp.ReleaseIPs(ctx, v6.IP) // release the unused paired v6
		}
	}

	// Release the OLD IP of this family (the other family is untouched).
	oldFam := curV4
	if isV6 {
		oldFam = curV6
	}
	if oldFam != nil && !oldFam.IP.Equal(committed.IP) {
		_ = s.dhcp.ReleaseIPs(ctx, oldFam.IP)
	}

	hn := hostname
	if hn == "" && cur != nil {
		hn = cur.Hostname
	}
	newAlloc := &tunAllocation{IPv4: curV4, IPv6: curV6, Version: time.Now().UnixNano(), LastRenew: time.Now(), Hostname: hn}
	if isV6 {
		newAlloc.IPv6 = committed
	} else {
		newAlloc.IPv4 = committed
	}
	s.allocs[owner] = newAlloc
	s.clearProposalFamily(owner, isV6)
	if err := s.saveAllocs(ctx); err != nil {
		plog.G(ctx).Errorf("[TunConfig] commit: persist failed for owner %s: %v", owner, err)
		return nil, err
	}
	// newAlloc.Version was bumped to a fresh UnixNano above, so the operator's edit
	// (which carried the previous version) is now strictly older: any lagging read of it
	// is rejected by the stale-echo guard in proposeManualChange and cannot re-propose.
	plog.G(ctx).Infof("[TunConfig] committed TUN IP %v (v6=%t) for owner %s (client confirmed)", committed.IP, isV6, owner)
	// Do NOT notifyWatchers here: the confirming client IS this owner's watcher and
	// applies the returned resp inline. Echoing it over the stream would re-deliver a
	// stale snapshot of the *other* family (committed at a different instant), which
	// the client would re-apply and flap. The ticker re-push keeps other state fresh.
	go s.syncEnvoyRuleIP(context.Background(), owner, newAlloc.IPv4, newAlloc.IPv6)
	return tunResp(newAlloc), nil
}

// clearProposalFamily removes one family's slot from an owner's pending proposal,
// deleting the proposal entirely when both slots are empty. Caller holds s.mu.
func (s *TunConfigServer) clearProposalFamily(owner string, isV6 bool) {
	p, ok := s.pendingProposal[owner]
	if !ok {
		return
	}
	if isV6 {
		p.candV6 = nil
	} else {
		p.candV4 = nil
	}
	if p.candV4 == nil && p.candV6 == nil {
		delete(s.pendingProposal, owner)
	} else {
		s.pendingProposal[owner] = p
	}
}

func parseExcludeIPs(raw []string) []net.IP {
	ips := make([]net.IP, 0, len(raw))
	for _, s := range raw {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

func isIPExcluded(ipNet *net.IPNet, excludeIPs []net.IP) bool {
	if ipNet == nil {
		return false
	}
	for _, excluded := range excludeIPs {
		if ipNet.IP.Equal(excluded) {
			return true
		}
	}
	return false
}

// pendingProposalResp builds a DryRun snapshot of an owner's outstanding proposal,
// or nil if none. Used to re-deliver a pending proposal to a freshly (re)subscribed
// watcher. Caller holds s.mu.
func (s *TunConfigServer) pendingProposalResp(ownerID string) *rpc.TunIPResponse {
	p, ok := s.pendingProposal[ownerID]
	if !ok || (p.candV4 == nil && p.candV6 == nil) {
		return nil
	}
	resp := &rpc.TunIPResponse{Version: time.Now().UnixNano(), DryRun: true}
	if p.candV4 != nil {
		resp.IPv4 = p.candV4.String()
	}
	if p.candV6 != nil {
		resp.IPv6 = p.candV6.String()
	}
	return resp
}

// familyCandidate validates one address family's desired IP for an owner and returns
// the candidate to propose, or nil to skip (empty/unchanged/already-proposed/out-of-
// range/in-use). isV6 selects the family. Caller need not hold s.mu (reads snapshots).
func (s *TunConfigServer) familyCandidate(ctx context.Context, owner, wantStr string, curIP, pendCand *net.IPNet, current map[string]*tunAllocation, isV6 bool) *net.IPNet {
	want := cidrToIPNet(wantStr)
	if want == nil {
		if wantStr != "" {
			plog.G(ctx).Warnf("[TunConfig] manual TUN_ALLOCS IP %q for owner %s is not valid CIDR, ignoring", wantStr, owner)
		}
		return nil // empty field = this family is not being changed
	}
	if curIP != nil && curIP.IP.Equal(want.IP) {
		return nil // already committed at target
	}
	if pendCand != nil && pendCand.IP.Equal(want.IP) {
		return nil // already proposed this candidate — don't re-push
	}
	if !s.dhcp.InRange(want.IP) {
		plog.G(ctx).Warnf("[TunConfig] manual TUN_ALLOCS IP %s for owner %s is out of range, ignoring", want.IP, owner)
		return nil
	}
	// No silent takeover: if another live owner holds the target IP (same family), refuse.
	for b, bAlloc := range current {
		if b == owner {
			continue
		}
		held := bAlloc.IPv4
		if isV6 {
			held = bAlloc.IPv6
		}
		if held != nil && held.IP.Equal(want.IP) {
			plog.G(ctx).Warnf("[TunConfig] manual TUN_ALLOCS IP %s for owner %s is in use by owner %s, ignoring", want.IP, owner, b)
			s.mu.Lock()
			s.recordRejectedLocked(owner, want.IP, nil)
			s.mu.Unlock()
			go s.persistRejected(context.Background())
			return nil
		}
	}
	// Always commit a host mask, regardless of what a manual TUN_ALLOCS edit wrote
	// (e.g. "x/16") — leases identify one host, never the pool.
	mask := net.CIDRMask(32, 32)
	if isV6 {
		mask = net.CIDRMask(128, 128)
	}
	return &net.IPNet{IP: want.IP, Mask: mask}
}

// allocateForOwner allocates a TUN IP for ownerID, preferring the IP the owner
// most recently held (when remembered and not excluded) so reconnects are
// sticky, and falling back to the next available IP otherwise. Caller holds s.mu.
func (s *TunConfigServer) allocateForOwner(ctx context.Context, ownerID string, excludeIPs []net.IP) (*net.IPNet, *net.IPNet, error) {
	if rec, ok := s.lastIPs[ownerID]; ok && rec.v4 != nil && !isIPExcluded(rec.v4, excludeIPs) {
		var prefV6 net.IP
		if rec.v6 != nil {
			prefV6 = rec.v6.IP
		}
		return s.dhcp.RentIPPreferring(ctx, rec.v4.IP, prefV6, excludeIPs)
	}
	return s.dhcp.RentIPExcluding(ctx, excludeIPs)
}

// rollbackAlloc releases a freshly-rented IP and drops its in-memory record after
// a persistence failure, so a bitmap bit is never left without an owning lease
// (which would never be renewed or reaped). Caller must hold s.mu.
func (s *TunConfigServer) rollbackAlloc(ctx context.Context, ownerID string, v4, v6 *net.IPNet) {
	delete(s.allocs, ownerID)
	var ip4, ip6 net.IP
	if v4 != nil {
		ip4 = v4.IP
	}
	if v6 != nil {
		ip6 = v6.IP
	}
	if err := s.dhcp.ReleaseIP(ctx, ip4, ip6); err != nil {
		plog.G(ctx).Errorf("[TunConfig] rollback: failed to release %v/%v: %v", ip4, ip6, err)
	}
}

// cidrToIPNet parses "ip/mask" into an *net.IPNet whose IP is the host address
// (not the masked network address). Returns nil on empty input or parse failure.
func cidrToIPNet(s string) *net.IPNet {
	if s == "" {
		return nil
	}
	ip, ipNet, err := net.ParseCIDR(s)
	if err != nil || ipNet == nil {
		return nil
	}
	ipNet.IP = ip
	return ipNet
}

