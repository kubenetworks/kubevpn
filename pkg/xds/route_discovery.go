package xds

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	informerv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// routeDiscoveryDebounce coalesces bursts of pod/service informer events into a
// single recompute+broadcast, so a rollout that churns many pods does not push a
// delta per event.
const routeDiscoveryDebounce = 300 * time.Millisecond

// routeSubBuffer is the per-subscriber channel depth. A slow client that fills it
// drops frames (non-blocking send); it re-syncs via a fresh snapshot on reconnect.
const routeSubBuffer = 4

// podIPToPrefix aggregates a pod IP to a fixed-granularity route prefix: IPv4 -> /24,
// IPv6 -> /64. This collapses the many per-pod /32 the client used to route into a
// handful of prefixes (≈ the node podCIDRs the namespace's pods span) while bounding
// over-capture to cluster pod space (the target is the TUN device, exactly as the
// whole detected pod CIDR is already routed). It deliberately does NOT do minimal
// aggregation of arbitrary IPs, which could summarise into a prefix wide enough to
// swallow non-cluster ranges.
func podIPToPrefix(ip net.IP) (string, bool) {
	if ip == nil {
		return "", false
	}
	if v4 := ip.To4(); v4 != nil {
		mask := net.CIDRMask(24, 32)
		return (&net.IPNet{IP: v4.Mask(mask), Mask: mask}).String(), true
	}
	mask := net.CIDRMask(64, 128)
	return (&net.IPNet{IP: ip.Mask(mask), Mask: mask}).String(), true
}

// stripPodTransform keeps only the fields the route discovery needs (Name/Namespace
// for the cache key, HostNetwork to exclude host-net pods, and the pod IPs), so the
// per-namespace pod informer cache holds tiny objects instead of full Pod specs.
// UID/ResourceVersion are intentionally dropped: the reflector tracks resource
// version from the List/Watch response metadata and raw watch events (the transform
// runs on the DeltaFIFO after that), and the cache keys by namespace/name only.
func stripPodTransform(obj any) (any, error) {
	if _, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return obj, nil
	}
	p, ok := obj.(*v1.Pod)
	if !ok {
		return obj, nil
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		Spec:       v1.PodSpec{HostNetwork: p.Spec.HostNetwork},
		Status:     v1.PodStatus{PodIP: p.Status.PodIP, PodIPs: p.Status.PodIPs},
	}, nil
}

// stripSvcTransform keeps only the fields DNS/routing needs (Name/Namespace, the
// ClusterIPs, ExternalName), dropping ports/selector/status/etc.
func stripSvcTransform(obj any) (any, error) {
	if _, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return obj, nil
	}
	s, ok := obj.(*v1.Service)
	if !ok {
		return obj, nil
	}
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace},
		Spec: v1.ServiceSpec{
			ClusterIP:    s.Spec.ClusterIP,
			ClusterIPs:   s.Spec.ClusterIPs,
			ExternalName: s.Spec.ExternalName,
		},
	}, nil
}

// routeSnapshot is the full route/DNS state for one namespace.
type routeSnapshot struct {
	podCIDRs map[string]struct{}      // set of aggregated pod prefixes
	services map[string]*rpc.ServiceRecord // key "namespace/name"
}

func newRouteSnapshot() routeSnapshot {
	return routeSnapshot{podCIDRs: map[string]struct{}{}, services: map[string]*rpc.ServiceRecord{}}
}

// nsRouteHub owns the pod+service informers for a single namespace and fans out
// snapshot/delta frames to all subscribers of that namespace.
type nsRouteHub struct {
	ns      string
	enabled bool // false => manager lacks RBAC to watch this namespace

	podInformer cache.SharedIndexInformer
	svcInformer cache.SharedIndexInformer
	stop        chan struct{}

	mu       sync.Mutex
	subs     map[chan *rpc.NamespaceRoutesResponse]struct{}
	last     routeSnapshot
	version  int64
	debounce *time.Timer
}

// routeBroadcaster manages one nsRouteHub per subscribed namespace, creating them
// lazily on first subscribe and garbage-collecting them when the last subscriber leaves.
type routeBroadcaster struct {
	clientset kubernetes.Interface

	mu   sync.Mutex
	hubs map[string]*nsRouteHub
}

func newRouteBroadcaster(clientset kubernetes.Interface) *routeBroadcaster {
	return &routeBroadcaster{clientset: clientset, hubs: map[string]*nsRouteHub{}}
}

// subscribe registers a channel for the namespace, creating the hub (and its
// informers) on first use. It returns the channel (pre-loaded with a Snapshot
// frame) and an unsubscribe func. ctx bounds the initial RBAC probe / cache sync.
func (b *routeBroadcaster) subscribe(ctx context.Context, ns string) (chan *rpc.NamespaceRoutesResponse, func()) {
	b.mu.Lock()
	hub := b.hubs[ns]
	if hub == nil {
		hub = b.newHub(ctx, ns)
		b.hubs[ns] = hub
	}
	b.mu.Unlock()

	ch := make(chan *rpc.NamespaceRoutesResponse, routeSubBuffer)

	hub.mu.Lock()
	hub.subs[ch] = struct{}{}
	// Deliver the current full state as the first frame so a new subscriber is
	// consistent with the delta stream that follows (both under hub.mu).
	ch <- hub.snapshotFrameLocked()
	hub.mu.Unlock()

	unsub := func() {
		hub.mu.Lock()
		delete(hub.subs, ch)
		empty := len(hub.subs) == 0
		hub.mu.Unlock()
		if empty {
			b.gc(ns, hub)
		}
	}
	return ch, unsub
}

// gc stops and removes a hub if it still has no subscribers.
func (b *routeBroadcaster) gc(ns string, hub *nsRouteHub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hub.mu.Lock()
	empty := len(hub.subs) == 0
	hub.mu.Unlock()
	if !empty || b.hubs[ns] != hub {
		return
	}
	if hub.stop != nil {
		close(hub.stop)
	}
	delete(b.hubs, ns)
}

// newHub creates a hub for ns. It probes RBAC first: if the manager cannot list
// pods/services in ns, the hub is disabled (subscribers get a single Enabled=false
// frame and no informers run) so the client degrades to CIDR-only routing.
func (b *routeBroadcaster) newHub(ctx context.Context, ns string) *nsRouteHub {
	hub := &nsRouteHub{
		ns:      ns,
		subs:    map[chan *rpc.NamespaceRoutesResponse]struct{}{},
		last:    newRouteSnapshot(),
		enabled: true,
	}
	if _, err := b.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1}); apierrors.IsForbidden(err) {
		plog.G(ctx).Warnf("[RouteDiscovery] no RBAC to list pods in namespace %q; serving Enabled=false", ns)
		hub.enabled = false
		return hub
	}

	hub.stop = make(chan struct{})
	hub.podInformer = informerv1.NewFilteredPodInformer(b.clientset, ns, 0, cache.Indexers{}, nil)
	hub.svcInformer = informerv1.NewFilteredServiceInformer(b.clientset, ns, 0, cache.Indexers{}, nil)
	_ = hub.podInformer.SetTransform(stripPodTransform)
	_ = hub.svcInformer.SetTransform(stripSvcTransform)

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { hub.scheduleRecompute() },
		UpdateFunc: func(any, any) { hub.scheduleRecompute() },
		DeleteFunc: func(any) { hub.scheduleRecompute() },
	}
	_, _ = hub.podInformer.AddEventHandler(handler)
	_, _ = hub.svcInformer.AddEventHandler(handler)

	go hub.podInformer.Run(hub.stop)
	go hub.svcInformer.Run(hub.stop)

	// Warm the cache before the first subscribe reads it, so the initial snapshot is
	// complete. Bounded by ctx; if it times out we still serve whatever synced.
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cache.WaitForCacheSync(syncCtx.Done(), hub.podInformer.HasSynced, hub.svcInformer.HasSynced)
	hub.mu.Lock()
	hub.last = hub.computeLocked()
	hub.mu.Unlock()
	return hub
}

// scheduleRecompute debounces informer events into a single recompute+broadcast.
func (h *nsRouteHub) scheduleRecompute() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.debounce != nil {
		return
	}
	h.debounce = time.AfterFunc(routeDiscoveryDebounce, func() {
		h.mu.Lock()
		h.debounce = nil
		h.broadcastDeltaLocked()
		h.mu.Unlock()
	})
}

// computeLocked builds the current full snapshot from the informer caches.
func (h *nsRouteHub) computeLocked() routeSnapshot {
	snap := newRouteSnapshot()
	if h.podInformer != nil {
		for _, obj := range h.podInformer.GetStore().List() {
			p, ok := obj.(*v1.Pod)
			if !ok || p.Spec.HostNetwork {
				continue
			}
			for _, ipStr := range podIPs(p) {
				if prefix, ok := podIPToPrefix(net.ParseIP(ipStr)); ok {
					snap.podCIDRs[prefix] = struct{}{}
				}
			}
		}
	}
	if h.svcInformer != nil {
		for _, obj := range h.svcInformer.GetStore().List() {
			s, ok := obj.(*v1.Service)
			if !ok {
				continue
			}
			rec := serviceRecord(s)
			snap.services[rec.Namespace+"/"+rec.Name] = rec
		}
	}
	return snap
}

// broadcastDeltaLocked recomputes the snapshot, diffs against last, and pushes a
// delta frame to all subscribers. Caller holds h.mu.
func (h *nsRouteHub) broadcastDeltaLocked() {
	next := h.computeLocked()
	resp := &rpc.NamespaceRoutesResponse{Enabled: true}
	for cidr := range next.podCIDRs {
		if _, ok := h.last.podCIDRs[cidr]; !ok {
			resp.AddedPodCIDRs = append(resp.AddedPodCIDRs, cidr)
		}
	}
	for cidr := range h.last.podCIDRs {
		if _, ok := next.podCIDRs[cidr]; !ok {
			resp.RemovedPodCIDRs = append(resp.RemovedPodCIDRs, cidr)
		}
	}
	for key, rec := range next.services {
		if prev, ok := h.last.services[key]; !ok || !sameServiceRecord(prev, rec) {
			resp.UpsertedServices = append(resp.UpsertedServices, rec)
		}
	}
	for key := range h.last.services {
		if _, ok := next.services[key]; !ok {
			resp.RemovedServiceKeys = append(resp.RemovedServiceKeys, key)
		}
	}
	h.last = next
	if len(resp.AddedPodCIDRs) == 0 && len(resp.RemovedPodCIDRs) == 0 &&
		len(resp.UpsertedServices) == 0 && len(resp.RemovedServiceKeys) == 0 {
		return
	}
	h.version++
	resp.Version = h.version
	for ch := range h.subs {
		select {
		case ch <- resp:
		default:
		}
	}
}

// snapshotFrameLocked builds a full Snapshot=true frame from current state. Caller holds h.mu.
func (h *nsRouteHub) snapshotFrameLocked() *rpc.NamespaceRoutesResponse {
	if !h.enabled {
		return &rpc.NamespaceRoutesResponse{Snapshot: true, Enabled: false}
	}
	resp := &rpc.NamespaceRoutesResponse{Snapshot: true, Enabled: true, Version: h.version}
	for cidr := range h.last.podCIDRs {
		resp.AddedPodCIDRs = append(resp.AddedPodCIDRs, cidr)
	}
	for _, rec := range h.last.services {
		resp.UpsertedServices = append(resp.UpsertedServices, rec)
	}
	sort.Strings(resp.AddedPodCIDRs)
	return resp
}

// podIPs returns the pod's IPs from Status (PodIPs preferred, PodIP fallback).
func podIPs(p *v1.Pod) []string {
	if len(p.Status.PodIPs) > 0 {
		out := make([]string, 0, len(p.Status.PodIPs))
		for _, ip := range p.Status.PodIPs {
			if ip.IP != "" {
				out = append(out, ip.IP)
			}
		}
		return out
	}
	if p.Status.PodIP != "" {
		return []string{p.Status.PodIP}
	}
	return nil
}

// serviceRecord builds the wire ServiceRecord for a service, folding ClusterIP into
// ClusterIPs and dropping headless ("None")/empty entries.
func serviceRecord(s *v1.Service) *rpc.ServiceRecord {
	seen := map[string]struct{}{}
	var ips []string
	for _, ip := range append([]string{s.Spec.ClusterIP}, s.Spec.ClusterIPs...) {
		if ip == "" || ip == v1.ClusterIPNone {
			continue
		}
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	return &rpc.ServiceRecord{
		Name:         s.Name,
		Namespace:    s.Namespace,
		ClusterIPs:   ips,
		ExternalName: s.Spec.ExternalName,
	}
}

func sameServiceRecord(a, b *rpc.ServiceRecord) bool {
	if a.ExternalName != b.ExternalName || len(a.ClusterIPs) != len(b.ClusterIPs) {
		return false
	}
	for i := range a.ClusterIPs {
		if a.ClusterIPs[i] != b.ClusterIPs[i] {
			return false
		}
	}
	return true
}

// WatchNamespaceRoutes streams pod route prefixes and service records for a namespace.
func (s *TunConfigServer) WatchNamespaceRoutes(req *rpc.NamespaceRoutesRequest, stream rpc.TunConfigService_WatchNamespaceRoutesServer) error {
	ch, unsub := s.routes.subscribe(stream.Context(), req.GetNamespace())
	defer unsub()
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
