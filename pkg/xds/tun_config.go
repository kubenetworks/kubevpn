package xds

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// annoTunAllocsRejected is the ConfigMap annotation key carrying the breadcrumb for
// a manual TUN_ALLOCS edit that was not applied (client conflict, IP in use, expiry).
const annoTunAllocsRejected = "kubevpn.io/tun-allocs-rejected"

// TunConfigServer implements the TunConfigService gRPC API.
// It manages TUN IP allocations for sidecars and pushes changes via streams.
type TunConfigServer struct {
	rpc.UnimplementedTunConfigServiceServer

	clientset kubernetes.Interface
	namespace string
	dhcp      *dhcp.Manager

	mu       sync.RWMutex
	allocs   map[string]*tunAllocation // ownerID → allocation
	watchers map[string][]chan *rpc.TunIPResponse
	// lastIPs remembers the IP each ownerID most recently held, even after its
	// lease was reaped, so a reconnecting (stable) owner can reclaim the same IP
	// when it is still free. In-memory only; bounded by the real client count
	// because OwnerID is now stable per machine+user.
	lastIPs map[string]lastIPRecord

	// reapedAt records when an ownerID's lease was reaped (and not since re-acquired).
	// The lease reaper uses it to clean up a truly-abandoned owner's envoy rules after
	// abandonmentTTL — long enough that a sleeping client (whose lease lapsed but which
	// will wake and re-allocate) is NOT affected. Cleared when the owner calls GetTunIP.
	reapedAt map[string]time.Time

	// pendingProposal tracks a dry-run IP change proposed to a client but not yet
	// confirmed. It is pure intent — NO IP is rented while pending. The server never
	// commits an IP change until the client confirms (via GetTunIP{ConfirmIP}), so a
	// conflicting proposal is simply dropped (decline/expiry) with nothing to roll
	// back. In-memory only — lost on restart (proposal vanishes; operator re-edits).
	pendingProposal map[string]proposal

	// rejectedAnno is the latest "manual TUN_ALLOCS edit not applied" breadcrumb.
	// It is written into the ConfigMap by saveAllocs (the single CM writer), so it
	// can never be clobbered by a concurrent allocation persist — every saveAllocs
	// re-applies it. Empty means no rejection to surface. Guarded by mu.
	rejectedAnno string

	// reconcileMu serializes ReconcileAllocsFromConfigMap so two overlapping
	// informer-driven reconciles never act on each other's stale snapshot.
	reconcileMu sync.Mutex

	// routes serves WatchNamespaceRoutes: server-side per-namespace pod/service
	// discovery pushed to clients, replacing each client's cluster-wide list-watch.
	routes *routeBroadcaster
}

// lastIPRecord is the last IPv4/IPv6 a given owner held.
type lastIPRecord struct {
	v4 *net.IPNet
	v6 *net.IPNet
}

// proposal is a dry-run IP change offered to a client, awaiting confirm/decline.
// candV4/candV6 are per-family candidates (either may be nil = that family is not
// being changed). They hold no bitmap reservation; an IP is rented only at confirm.
type proposal struct {
	candV4   *net.IPNet
	candV6   *net.IPNet
	deadline time.Time
}

type tunAllocation struct {
	IPv4      *net.IPNet
	IPv6      *net.IPNet
	Version   int64
	LastRenew time.Time // last time the client renewed (heartbeat)
	Hostname  string    // client machine name, for debugging which machine owns this lease
}

// persistedAlloc is the YAML-serializable form of tunAllocation.
//
// Tags are json (not yaml): saveAllocs marshals via sigs.k8s.io/yaml, which encodes
// through encoding/json and ignores yaml tags. The keys are therefore honest here —
// they are what actually lands in the TUN_ALLOCS ConfigMap — and omitempty works, so
// a client that sends no hostname leaves a clean entry. Reads are case-insensitive,
// so records written by older builds (capitalized Go-name keys) still load.
type persistedAlloc struct {
	IPv4      string `json:"ipv4"`
	IPv6      string `json:"ipv6"`
	Version   int64  `json:"version"`
	LastRenew int64  `json:"lastRenew"` // unix timestamp
	Hostname  string `json:"hostname,omitempty"`
}

// initDHCPBackoff controls how NewTunConfigServer retries a transient InitDHCP
// failure. A freshly created traffic-manager pod may reach its xds container
// before the pod network / kube-proxy is ready, so the very first ConfigMap read
// can hit "connect: connection refused" to the API server ClusterIP. That window
// closes within seconds, so a bounded backoff (~1 minute total) lets init succeed
// on retry instead of the server coming up permanently without TunConfigService.
// Package-level so tests can shrink it.
var initDHCPBackoff = wait.Backoff{
	Duration: 1 * time.Second,
	Factor:   1.5,
	Steps:    8,
	Cap:      15 * time.Second,
}

// NewTunConfigServer creates and initializes a TunConfigServer.
// Performs DHCP init and loads persisted allocations from ConfigMap.
// Server restart does not affect clients — they keep their same IPs.
//
// InitDHCP is a hard prerequisite for the data plane: without it the caller must
// NOT register TunConfigService (clients would get "unknown service" forever).
// A transient API-connectivity error at fresh-pod startup is therefore retried,
// and only a persistent failure is returned — the caller treats that as fatal so
// the container restarts and retries from a clean state.
func NewTunConfigServer(ctx context.Context, clientset kubernetes.Interface, namespace string) (*TunConfigServer, error) {
	mgr := dhcp.NewDHCPManager(clientset, namespace)
	if err := initDHCPWithRetry(ctx, mgr); err != nil {
		return nil, fmt.Errorf("init DHCP: %w", err)
	}
	s := &TunConfigServer{
		clientset:       clientset,
		namespace:       namespace,
		dhcp:            mgr,
		allocs:          make(map[string]*tunAllocation),
		watchers:        make(map[string][]chan *rpc.TunIPResponse),
		lastIPs:         make(map[string]lastIPRecord),
		reapedAt:        make(map[string]time.Time),
		pendingProposal: make(map[string]proposal),
	}
	s.routes = newRouteBroadcaster(clientset)
	s.loadAllocs(ctx)
	// Reclaim any bitmap bits left orphaned by a prior crash before serving.
	s.scrubOrphanBits(ctx)
	return s, nil
}

// initDHCPWithRetry runs mgr.InitDHCP with a bounded exponential backoff, so a
// transient API-server connectivity blip at fresh-pod startup does not leave the
// xds server permanently without TunConfigService. It returns the last InitDHCP
// error (not the generic backoff-timeout) so the caller can log why it gave up.
func initDHCPWithRetry(ctx context.Context, mgr *dhcp.Manager) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, initDHCPBackoff, func(ctx context.Context) (bool, error) {
		if err := mgr.InitDHCP(ctx); err != nil {
			lastErr = err
			plog.G(ctx).Warnf("[TunConfig] InitDHCP failed, will retry: %v", err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if lastErr != nil {
			return lastErr
		}
		return err
	}
	return nil
}

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

// syncEnvoyRuleIP updates Rule.LocalTunIPv4/v6 in ENVOY_CONFIG for all Rules matching ownerID.
// This triggers: Watcher → Processor → xDS push → envoy hot-update.
func (s *TunConfigServer) syncEnvoyRuleIP(ctx context.Context, ownerID string, newIPv4, newIPv6 *net.IPNet) {
	newV4Str := ""
	if newIPv4 != nil {
		newV4Str = newIPv4.IP.String()
	}
	newV6Str := ""
	if newIPv6 != nil {
		newV6Str = newIPv6.IP.String()
	}

	skipped := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		virtuals, parseErr := parseYaml(cm.Data[config.KeyEnvoy])
		if parseErr != nil {
			// ENVOY_CONFIG is not a valid Virtual list (empty/legacy/corrupt) — there
			// are no rules to sync. Skip rather than error: a hard failure here cannot
			// be retried into success and only spams the log.
			plog.G(ctx).Warnf("[TunConfig] syncEnvoyRuleIP: skip owner %s, cannot parse %s: %v", ownerID, config.KeyEnvoy, parseErr)
			skipped = true
			return nil
		}

		changed := false
		for _, v := range virtuals {
			for _, rule := range v.Rules {
				if rule.OwnerID == ownerID && rule.LocalTunIPv4 != newV4Str {
					rule.LocalTunIPv4 = newV4Str
					rule.LocalTunIPv6 = newV6Str
					changed = true
				}
			}
		}
		if !changed {
			return nil
		}

		data, marshalErr := yaml.Marshal(virtuals)
		if marshalErr != nil {
			return marshalErr
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyEnvoy] = string(data)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Errorf("[TunConfig] syncEnvoyRuleIP failed for owner %s: %v", ownerID, err)
	} else if !skipped {
		plog.G(ctx).Infof("[TunConfig] Synced envoy rule IP for owner %s to %v", ownerID, newIPv4)
	}
}

// XDSPort is the gRPC port used by the envoy control plane and TunConfigService.
const XDSPort uint = config.PortXDS

// LeaseDuration is how long a TUN IP allocation stays valid without renewal.
// If a client doesn't call GetTunIP (which doubles as renew) within this duration,
// the IP is reclaimed and recycled.
const LeaseDuration = 5 * time.Minute

// leaseReapInterval is how often the lease reaper scans for and reclaims expired allocations.
const leaseReapInterval = 30 * time.Second

// abandonmentTTL is how long after a lease is reaped (with no re-acquire) the owner's
// envoy rules are cleaned up as abandoned. It is deliberately much longer than
// LeaseDuration so a sleeping client — whose lease lapses but which will wake and
// re-allocate, re-pointing its still-present rule via syncEnvoyRuleIP — is not affected;
// only a client gone this long (crash / SIGKILL / powered off) has its rule removed.
// Default 24h; override with KUBEVPN_PROXY_ABANDON_TTL (a Go duration, e.g. "6h").
var abandonmentTTL = resolveAbandonmentTTL()

func resolveAbandonmentTTL() time.Duration {
	if v := os.Getenv("KUBEVPN_PROXY_ABANDON_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 24 * time.Hour
}

// removeEnvoyRulesForOwner deletes all envoy rules owned by ownerID from ENVOY_CONFIG
// (dropping any Virtual left with no rules). Used by the lease reaper's abandonment pass
// to clean up a rule left behind by a client that vanished without a clean Leave (crash /
// SIGKILL). It mirrors syncEnvoyRuleIP's read-modify-write; the xDS ConfigMap watcher
// re-pushes the envoy snapshot on the change. It does NOT unpatch the sidecar container
// (that needs workload RBAC).
func (s *TunConfigServer) removeEnvoyRulesForOwner(ctx context.Context, ownerID string) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		virtuals, parseErr := parseYaml(cm.Data[config.KeyEnvoy])
		if parseErr != nil {
			return nil // not a valid Virtual list (empty/legacy/corrupt): nothing to remove
		}
		changed := false
		kept := virtuals[:0]
		for _, v := range virtuals {
			rules := v.Rules[:0]
			for _, rule := range v.Rules {
				if rule.OwnerID == ownerID {
					changed = true
					continue
				}
				rules = append(rules, rule)
			}
			v.Rules = rules
			if len(v.Rules) == 0 {
				continue // drop a Virtual with no remaining rules
			}
			kept = append(kept, v)
		}
		if !changed {
			return nil
		}
		data, marshalErr := yaml.Marshal(kept)
		if marshalErr != nil {
			return marshalErr
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyEnvoy] = string(data)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] failed to remove envoy rules for abandoned owner %s: %v", ownerID, err)
	} else {
		plog.G(ctx).Infof("[TunConfig] removed envoy rules for abandoned owner %s (no re-acquire in %v)", ownerID, abandonmentTTL)
	}
}

// manualGrace is how long a manual TUN_ALLOCS edit keeps the owner's pre-edit IP
// rented (a reservation) after pushing the new IP, awaiting client accept/reject.
// On expiry with no reject the change is assumed accepted and the old IP released.
const manualGrace = 60 * time.Second

// maxReconcileRounds bounds the re-read/merge loop in ReconcileAllocsFromConfigMap
// that absorbs concurrent operator edits landing during a reconcile.
const maxReconcileRounds = 5

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
