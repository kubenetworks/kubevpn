package controlplane

import (
	"context"
	"net"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

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

// NewTunConfigServer creates a TunConfigServer with the given K8s client and DHCP manager.
func NewTunConfigServer(clientset kubernetes.Interface, namespace string) *TunConfigServer {
	mgr := dhcp.NewDHCPManager(clientset, namespace)
	return &TunConfigServer{
		clientset: clientset,
		namespace: namespace,
		dhcp:      mgr,
		allocs:    make(map[string]*tunAllocation),
		watchers:  make(map[string][]chan *rpc.TunIPResponse),
	}
}

// Init initializes the underlying DHCP manager.
func (s *TunConfigServer) Init(ctx context.Context) error {
	return s.dhcp.InitDHCP(ctx)
}

// GetTunIP allocates or retrieves the TUN IP for the given owner.
func (s *TunConfigServer) GetTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if alloc, ok := s.allocs[req.OwnerID]; ok {
		alloc.LastRenew = time.Now()
		return &rpc.TunIPResponse{
			IPv4:    alloc.IPv4.String(),
			IPv6:    alloc.IPv6.String(),
			Version: alloc.Version,
		}, nil
	}

	v4, v6, err := s.dhcp.RentIP(ctx)
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
	return &rpc.TunIPResponse{
		IPv4:    v4.String(),
		IPv6:    v6.String(),
		Version: alloc.Version,
	}, nil
}

// WatchTunIP streams IP changes to the caller. Blocks until context is cancelled.
func (s *TunConfigServer) WatchTunIP(req *rpc.TunIPRequest, stream rpc.TunConfigService_WatchTunIPServer) error {
	ch := make(chan *rpc.TunIPResponse, 4)

	s.mu.Lock()
	s.watchers[req.OwnerID] = append(s.watchers[req.OwnerID], ch)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.removeWatcher(req.OwnerID, ch)
		s.mu.Unlock()
	}()

	for {
		select {
		case resp := <-ch:
			if err := stream.Send(resp); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

// ReleaseTunIP releases the IP allocation for the given owner.
func (s *TunConfigServer) ReleaseTunIP(ctx context.Context, req *rpc.TunIPRequest) (*rpc.TunIPResponse, error) {
	s.mu.Lock()
	alloc, ok := s.allocs[req.OwnerID]
	if ok {
		delete(s.allocs, req.OwnerID)
	}
	s.mu.Unlock()

	if !ok {
		return &rpc.TunIPResponse{}, nil
	}

	var ipv4, ipv6 net.IP
	if alloc.IPv4 != nil {
		ipv4 = alloc.IPv4.IP
	}
	if alloc.IPv6 != nil {
		ipv6 = alloc.IPv6.IP
	}
	if err := s.dhcp.ReleaseIP(ctx, ipv4, ipv6); err != nil {
		plog.G(ctx).Errorf("[TunConfig] Failed to release IP for %s: %v", req.OwnerID, err)
	} else {
		plog.G(ctx).Infof("[TunConfig] Released IP for owner %s", req.OwnerID)
	}
	return &rpc.TunIPResponse{IPv4: alloc.IPv4.String(), IPv6: alloc.IPv6.String()}, nil
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

	resp := &rpc.TunIPResponse{
		IPv4:    newIPv4.String(),
		IPv6:    newIPv6.String(),
		Version: alloc.Version,
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

	for ownerID, alloc := range owners {
		if alloc.IPv4 != nil && !allocatedIPs[alloc.IPv4.IP.String()] {
			plog.G(ctx).Warnf("[TunConfig] IP %s for owner %s lost, re-allocating", alloc.IPv4, ownerID)
			newV4, newV6, err := s.dhcp.RentIP(ctx)
			if err != nil {
				plog.G(ctx).Errorf("[TunConfig] Failed to re-rent for %s: %v", ownerID, err)
				continue
			}
			s.NotifyIPChange(ownerID, newV4, newV6)
		}
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
}
