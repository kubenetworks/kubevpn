package controlplane

import (
	"context"
	"net"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	mu        sync.RWMutex
	allocs    map[string]*tunAllocation // ownerID → allocation
	watchers  map[string][]chan *rpc.TunIPResponse
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
			_ = s.dhcp.ReleaseIP(ctx, v4Net.IP, ipv6)
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
		go s.saveAllocs(context.Background())
	}
}

// saveAllocs persists the current allocs map to ConfigMap.
func (s *TunConfigServer) saveAllocs(ctx context.Context) {
	s.mu.RLock()
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
	s.mu.RUnlock()

	data, err := yaml.Marshal(persisted)
	if err != nil {
		return
	}

	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[config.KeyTunAllocs] = string(data)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
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
		// Old IP stays in DHCP bitmap so RentIPExcluding naturally skips it.
		delete(s.allocs, req.OwnerID)
		plog.G(ctx).Infof("[TunConfig] IP %v conflicts with client ExcludeIPs, re-allocating for owner %s", alloc.IPv4, req.OwnerID)

		s.mu.Unlock()
		v4, v6, err := s.dhcp.RentIPExcluding(ctx, excludeIPs)
		if err != nil {
			s.mu.Lock()
			return nil, err
		}
		// Release old IP now that we have a new one
		var oldV4, oldV6 net.IP
		if alloc.IPv4 != nil {
			oldV4 = alloc.IPv4.IP
		}
		if alloc.IPv6 != nil {
			oldV6 = alloc.IPv6.IP
		}
		_ = s.dhcp.ReleaseIP(ctx, oldV4, oldV6)
		s.mu.Lock()

		newAlloc := &tunAllocation{
			IPv4:      v4,
			IPv6:      v6,
			Version:   time.Now().UnixNano(),
			LastRenew: time.Now(),
		}
		s.allocs[req.OwnerID] = newAlloc
		plog.G(ctx).Infof("[TunConfig] Re-allocated %s/%s for owner %s", v4, v6, req.OwnerID)
		go s.saveAllocs(context.Background())

		return &rpc.TunIPResponse{
			IPv4:    v4.String(),
			IPv6:    v6.String(),
			Version: newAlloc.Version,
		}, nil
	}

	// New allocation
	s.mu.Unlock()
	v4, v6, err := s.dhcp.RentIPExcluding(ctx, excludeIPs)
	s.mu.Lock()
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
	go s.saveAllocs(context.Background())

	return &rpc.TunIPResponse{
		IPv4:    v4.String(),
		IPv6:    v6.String(),
		Version: alloc.Version,
	}, nil
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
			s.mu.Unlock()
		case <-stream.Context().Done():
			return nil
		}
	}
}


// NotifyIPChange is called by the ConfigMap watcher when an owner's IP changes.
// It updates the allocation and pushes to all watchers.
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

	for _, ch := range s.watchers[ownerID] {
		select {
		case ch <- resp:
		default:
		}
	}
	s.mu.Unlock()
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
		s.saveAllocs(ctx)
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

// ControlPlanePort is the gRPC port used by the envoy control plane and TunConfigService.
const ControlPlanePort uint = 9002

// LeaseDuration is how long a TUN IP allocation stays valid without renewal.
// If a client doesn't call GetTunIP (which doubles as renew) within this duration,
// the IP is reclaimed and recycled.
const LeaseDuration = 5 * time.Minute

// StartLeaseReaper launches a background goroutine that periodically checks for
// expired allocations and reclaims them.
func (s *TunConfigServer) StartLeaseReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
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
		if !ok {
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
		_ = s.dhcp.ReleaseIP(ctx, ipv4, ipv6)
		plog.G(ctx).Infof("[TunConfig] Lease expired for owner %s, reclaimed IP %v", ownerID, alloc.IPv4)
	}

	if len(expired) > 0 {
		s.saveAllocs(ctx)
	}
}
