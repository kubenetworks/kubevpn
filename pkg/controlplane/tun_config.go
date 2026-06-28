package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

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
}

type tunAllocation struct {
	IPv4      *net.IPNet
	IPv6      *net.IPNet
	Version   int64
	LastRenew time.Time // last time the client renewed (heartbeat)
}

// persistedAlloc is the YAML-serializable form of tunAllocation.
type persistedAlloc struct {
	IPv4      string `yaml:"ipv4"`
	IPv6      string `yaml:"ipv6"`
	Version   int64  `yaml:"version"`
	LastRenew int64  `yaml:"lastRenew"` // unix timestamp
}

// NewTunConfigServer creates and initializes a TunConfigServer.
// Performs DHCP init and loads persisted allocations from ConfigMap.
// Server restart does not affect clients — they keep their same IPs.
func NewTunConfigServer(ctx context.Context, clientset kubernetes.Interface, namespace string) (*TunConfigServer, error) {
	mgr := dhcp.NewDHCPManager(clientset, namespace)
	if err := mgr.InitDHCP(ctx); err != nil {
		return nil, err
	}
	s := &TunConfigServer{
		clientset: clientset,
		namespace: namespace,
		dhcp:      mgr,
		allocs:    make(map[string]*tunAllocation),
		watchers:  make(map[string][]chan *rpc.TunIPResponse),
	}
	s.loadAllocs(ctx)
	// Reclaim any bitmap bits left orphaned by a prior crash before serving.
	s.scrubOrphanBits(ctx)
	return s, nil
}

// loadAllocs reads the persisted ownerID→IP mapping from the ConfigMap.
func (s *TunConfigServer) loadAllocs(ctx context.Context) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil || cm.Data == nil {
		return
	}
	data := cm.Data[config.KeyTunAllocs]
	if data == "" {
		return
	}
	var persisted map[string]*persistedAlloc
	if err := yaml.Unmarshal([]byte(data), &persisted); err != nil {
		plog.G(ctx).Warnf("[TunConfig] Failed to load persisted allocs: %v", err)
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

	if alloc, ok := s.allocs[req.OwnerID]; ok {
		if !isIPExcluded(alloc.IPv4, excludeIPs) {
			alloc.LastRenew = time.Now()
			resp := &rpc.TunIPResponse{Version: alloc.Version}
			if alloc.IPv4 != nil {
				resp.IPv4 = alloc.IPv4.String()
			}
			if alloc.IPv6 != nil {
				resp.IPv6 = alloc.IPv6.String()
			}
			return resp, nil
		}
		// Existing IP conflicts with client's local interfaces — re-allocate.
		// Hold mutex across RentIP (fast ConfigMap operation).
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

		newAlloc := &tunAllocation{
			IPv4:      v4,
			IPv6:      v6,
			Version:   time.Now().UnixNano(),
			LastRenew: time.Now(),
		}
		s.allocs[req.OwnerID] = newAlloc
		plog.G(ctx).Infof("[TunConfig] Re-allocated %s/%s for owner %s", v4, v6, req.OwnerID)
		if err := s.saveAllocs(ctx); err != nil {
			plog.G(ctx).Errorf("[TunConfig] Failed to persist allocs, rolling back: %v", err)
			s.rollbackAlloc(ctx, req.OwnerID, v4, v6)
			return nil, err
		}

		resp := &rpc.TunIPResponse{IPv4: v4.String(), IPv6: v6.String(), Version: newAlloc.Version}
		s.notifyWatchers(req.OwnerID, resp)
		go s.syncEnvoyRuleIP(context.Background(), req.OwnerID, v4, v6)
		return resp, nil
	}

	// New allocation — hold mutex across RentIP (fast ConfigMap operation, no need to release)
	v4, v6, err := s.dhcp.RentIPExcluding(ctx, excludeIPs)
	if err != nil {
		return nil, err
	}

	alloc := &tunAllocation{
		IPv4:      v4,
		IPv6:      v6,
		Version:   time.Now().UnixNano(),
		LastRenew: time.Now(),
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

// WatchTunIP streams IP changes to the caller. Blocks until context is cancelled.
// An active stream acts as an implicit lease renewal — LastRenew is refreshed
// periodically so the LeaseReaper won't reclaim the IP while the stream is alive.
func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream rpc.TunConfigService_WatchTunIPServer) error {
	ch := make(chan *rpc.TunIPResponse, 4)

	s.mu.Lock()
	s.watchers[req.OwnerID] = append(s.watchers[req.OwnerID], ch)
	s.renewLease(req.OwnerID)
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
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		virtuals, parseErr := parseYaml(cm.Data[config.KeyEnvoy])
		if parseErr != nil {
			return parseErr
		}

		changed := false
		newV4Str := ""
		if newIPv4 != nil {
			newV4Str = newIPv4.IP.String()
		}
		newV6Str := ""
		if newIPv6 != nil {
			newV6Str = newIPv6.IP.String()
		}
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
		patch, patchErr := json.Marshal([]map[string]string{{
			"op":    "add",
			"path":  "/data/" + config.KeyEnvoy,
			"value": string(data),
		}})
		if patchErr != nil {
			return patchErr
		}
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Patch(ctx, config.ConfigMapPodTrafficManager, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Errorf("[TunConfig] syncEnvoyRuleIP failed for owner %s: %v", ownerID, err)
	} else {
		plog.G(ctx).Infof("[TunConfig] Synced envoy rule IP for owner %s to %v", ownerID, newIPv4)
	}
}

// ControlPlanePort is the gRPC port used by the envoy control plane and TunConfigService.
const ControlPlanePort uint = config.PortControlPlane

// LeaseDuration is how long a TUN IP allocation stays valid without renewal.
// If a client doesn't call GetTunIP (which doubles as renew) within this duration,
// the IP is reclaimed and recycled.
const LeaseDuration = 5 * time.Minute

// leaseReapInterval is how often the lease reaper scans for and reclaims expired allocations.
const leaseReapInterval = 30 * time.Second

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

	// Reclaim bitmap bits that no live allocation owns (e.g. leaked by a crash
	// between renting and persisting). Safe under the single-writer (Recreate)
	// deployment: GetTunIP is mutex-serialized, so no in-flight allocation is
	// misclassified as an orphan.
	s.scrubOrphanBits(ctx)
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
