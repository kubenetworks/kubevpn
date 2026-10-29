package xds

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
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

// routeDiscoveryInitTimeout covers both permission probes and the initial cache sync.
// Relists use the same budget, but active watches have only the hub lifetime as a bound.
const routeDiscoveryInitTimeout = 30 * time.Second

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
	podCIDRs map[string]struct{}           // set of aggregated pod prefixes
	services map[string]*rpc.ServiceRecord // key "namespace/name"
}

func newRouteSnapshot() routeSnapshot {
	return routeSnapshot{podCIDRs: map[string]struct{}{}, services: map[string]*rpc.ServiceRecord{}}
}

// nsRouteHub owns the pod+service informers for a single namespace and fans out
// snapshot/delta frames to all subscribers of that namespace.
type nsRouteHub struct {
	ns     string
	ctx    context.Context
	cancel context.CancelFunc

	// refs includes callers waiting for initialization as well as active streams;
	// it is protected by the broadcaster mutex, not mu. Closing ready publishes initErr.
	refs       int
	ready      chan struct{}
	initErr    error
	deadline   time.Time
	stopParent func() bool

	podInformer cache.SharedIndexInformer
	svcInformer cache.SharedIndexInformer

	mu       sync.Mutex
	synced   bool
	subs     map[chan *rpc.NamespaceRoutesResponse]struct{}
	last     routeSnapshot
	version  int64
	debounce *time.Timer
}

// routeBroadcaster manages one nsRouteHub per subscribed namespace, creating them
// lazily on first subscribe and garbage-collecting them when the last subscriber leaves.
type routeBroadcaster struct {
	ctx       context.Context
	clientset kubernetes.Interface

	mu   sync.Mutex
	hubs map[string]*nsRouteHub
}

func newRouteBroadcaster(ctx context.Context, clientset kubernetes.Interface) *routeBroadcaster {
	return &routeBroadcaster{ctx: ctx, clientset: clientset, hubs: map[string]*nsRouteHub{}}
}

// subscribe reserves a reference before waiting, so cancellation/GC cannot stop
// a hub another caller is about to use. Kubernetes work never holds b.mu.
func (b *routeBroadcaster) subscribe(ctx context.Context, ns string) (chan *rpc.NamespaceRoutesResponse, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	if err := b.ctx.Err(); err != nil {
		b.mu.Unlock()
		return nil, nil, err
	}
	hub := b.hubs[ns]
	created := hub == nil
	if created {
		hubCtx, cancel := context.WithCancel(b.ctx)
		hub = &nsRouteHub{
			ns: ns, ctx: hubCtx, cancel: cancel,
			ready: make(chan struct{}), deadline: time.Now().Add(routeDiscoveryInitTimeout),
			subs: map[chan *rpc.NamespaceRoutesResponse]struct{}{}, last: newRouteSnapshot(),
		}
		// Parent cancellation also detaches the hub even if a stream's send is
		// blocked. The callback takes b.mu, so it cannot run before assignment.
		hub.stopParent = context.AfterFunc(b.ctx, func() {
			b.mu.Lock()
			if b.hubs[ns] == hub {
				delete(b.hubs, ns)
			}
			b.mu.Unlock()
			hub.stop()
		})
		b.hubs[ns] = hub
	}
	hub.refs++
	b.mu.Unlock()
	if created {
		go b.initialize(hub)
	}

	select {
	case <-ctx.Done():
		b.release(hub)
		return nil, nil, ctx.Err()
	case <-b.ctx.Done():
		b.release(hub)
		return nil, nil, b.ctx.Err()
	case <-hub.ready:
	}
	if err := ctx.Err(); err != nil {
		b.release(hub)
		return nil, nil, err
	}
	if hub.initErr != nil {
		b.release(hub)
		return nil, nil, hub.initErr
	}
	if err := hub.ctx.Err(); err != nil {
		b.release(hub)
		return nil, nil, err
	}

	ch := make(chan *rpc.NamespaceRoutesResponse, routeSubBuffer)
	hub.mu.Lock()
	hub.subs[ch] = struct{}{}
	ch <- hub.snapshotFrameLocked()
	hub.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			hub.mu.Lock()
			delete(hub.subs, ch)
			hub.mu.Unlock()
			b.release(hub)
		})
	}
	return ch, unsub, nil
}

func (b *routeBroadcaster) release(hub *nsRouteHub) {
	b.mu.Lock()
	hub.refs--
	last := hub.refs == 0
	if last && b.hubs[hub.ns] == hub {
		delete(b.hubs, hub.ns)
	}
	b.mu.Unlock()
	if last {
		hub.stop()
	}
}

func (h *nsRouteHub) stop() {
	h.stopParent()
	h.cancel()
	h.mu.Lock()
	if h.debounce != nil {
		h.debounce.Stop()
		h.debounce = nil
	}
	h.mu.Unlock()
}

// initialize alone publishes ready/initErr. Failed generations are evicted
// before publication, allowing a new subscriber to retry without being removed
// by a late release from this generation.
func (b *routeBroadcaster) initialize(hub *nsRouteHub) {
	initCtx, cancel := context.WithDeadline(hub.ctx, hub.deadline)
	err := b.initHub(initCtx, hub)
	cancel()
	if err != nil {
		if hub.ctx.Err() == nil {
			plog.G(b.ctx).Warnf("[RouteDiscovery] initialize namespace %q failed: %v", hub.ns, err)
		}
		hub.stop()
		b.mu.Lock()
		if b.hubs[hub.ns] == hub {
			delete(b.hubs, hub.ns)
		}
		b.mu.Unlock()
	} else {
		hub.mu.Lock()
		plog.G(b.ctx).Debugf("[RouteDiscovery] namespace %q ready: %d services, %d pod CIDRs", hub.ns, len(hub.last.services), len(hub.last.podCIDRs))
		hub.mu.Unlock()
	}
	hub.initErr = err
	close(hub.ready)
}

func (b *routeBroadcaster) initHub(ctx context.Context, hub *nsRouteHub) error {
	pods := b.clientset.CoreV1().Pods(hub.ns)
	services := b.clientset.CoreV1().Services(hub.ns)
	if _, err := pods.List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list pods in namespace %q: %w", hub.ns, err)
	}
	if _, err := services.List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list services in namespace %q: %w", hub.ns, err)
	}

	hub.podInformer = cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			listCtx, cancel := hub.listContext()
			defer cancel()
			return pods.List(listCtx, opts)
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) { return pods.Watch(hub.ctx, opts) },
	}, &v1.Pod{}, 0, cache.Indexers{})
	hub.svcInformer = cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			listCtx, cancel := hub.listContext()
			defer cancel()
			return services.List(listCtx, opts)
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) { return services.Watch(hub.ctx, opts) },
	}, &v1.Service{}, 0, cache.Indexers{})
	if err := hub.podInformer.SetTransform(stripPodTransform); err != nil {
		return fmt.Errorf("set pod transform: %w", err)
	}
	if err := hub.svcInformer.SetTransform(stripSvcTransform); err != nil {
		return fmt.Errorf("set service transform: %w", err)
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { hub.scheduleRecompute() }, UpdateFunc: func(any, any) { hub.scheduleRecompute() }, DeleteFunc: func(any) { hub.scheduleRecompute() },
	}
	if _, err := hub.podInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("register pod handler: %w", err)
	}
	if _, err := hub.svcInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("register service handler: %w", err)
	}
	go hub.podInformer.Run(hub.ctx.Done())
	go hub.svcInformer.Run(hub.ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), hub.podInformer.HasSynced, hub.svcInformer.HasSynced) {
		return fmt.Errorf("sync namespace %q caches (pods=%t services=%t): %w", hub.ns, hub.podInformer.HasSynced(), hub.svcInformer.HasSynced(), ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	hub.mu.Lock()
	hub.last = hub.computeLocked()
	hub.synced = true
	hub.mu.Unlock()
	return nil
}

func (h *nsRouteHub) listContext() (context.Context, context.CancelFunc) {
	h.mu.Lock()
	synced := h.synced
	h.mu.Unlock()
	if !synced {
		return context.WithDeadline(h.ctx, h.deadline)
	}
	return context.WithTimeout(h.ctx, routeDiscoveryInitTimeout)
}

// scheduleRecompute debounces informer events into a single recompute+broadcast.
func (h *nsRouteHub) scheduleRecompute() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.synced || h.ctx.Err() != nil || h.debounce != nil {
		return
	}
	h.debounce = time.AfterFunc(routeDiscoveryDebounce, func() {
		h.mu.Lock()
		h.debounce = nil
		if h.ctx.Err() == nil {
			h.broadcastDeltaLocked()
		}
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
	ch, unsub, err := s.routes.subscribe(stream.Context(), req.GetNamespace())
	if err != nil {
		if apierrors.IsForbidden(err) {
			return stream.Send(&rpc.NamespaceRoutesResponse{Snapshot: true, Enabled: false})
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return status.FromContextError(err).Err()
		}
		return status.Error(codes.Unavailable, err.Error())
	}
	defer unsub()
	for {
		select {
		case resp := <-ch:
			if err := stream.Send(resp); err != nil {
				return err
			}
		case <-s.routes.ctx.Done():
			return status.FromContextError(s.routes.ctx.Err()).Err()
		case <-stream.Context().Done():
			return nil
		}
	}
}
