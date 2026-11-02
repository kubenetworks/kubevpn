package discovery

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coretyped "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestIntegration_CloseJoinsNamespaceInformers(t *testing.T) {
	env := newTestEnv(t)
	b := env.server.routes
	_, unsub, err := b.Subscribe(context.Background(), "test-ns")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	b.mu.Lock()
	hub := b.hubs["test-ns"]
	b.mu.Unlock()
	hub.scheduleRecompute()
	b.Close()
	if !hub.podInformer.IsStopped() || !hub.svcInformer.IsStopped() {
		t.Fatal("Close returned with an informer running")
	}
	b.mu.Lock()
	remaining := len(b.hubs)
	b.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("Close retained %d namespace hubs", remaining)
	}
	if _, _, err := b.Subscribe(context.Background(), "test-ns"); !errors.Is(err, context.Canceled) {
		t.Fatalf("subscribe after Close: %v", err)
	}
}

// Blocking inside fake.Clientset reactors would hold the fake's global mutex,
// artificially blocking DHCP too. Wrap just the typed API boundary instead.
type blockedDiscoveryClient struct {
	kubernetes.Interface
	resource   string
	cacheOnly  bool
	entered    chan context.Context
	release    chan struct{}
	finished   chan struct{}
	once       sync.Once
	finishOnce sync.Once
	openOnce   sync.Once
}

func (c *blockedDiscoveryClient) open() { c.openOnce.Do(func() { close(c.release) }) }

func (c *blockedDiscoveryClient) block(ctx context.Context) error {
	c.once.Do(func() { c.entered <- ctx })
	defer c.finishOnce.Do(func() { close(c.finished) })
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *blockedDiscoveryClient) CoreV1() coretyped.CoreV1Interface {
	return &blockedDiscoveryCore{CoreV1Interface: c.Interface.CoreV1(), client: c}
}

type blockedDiscoveryCore struct {
	coretyped.CoreV1Interface
	client *blockedDiscoveryClient
}

func (c *blockedDiscoveryCore) Pods(ns string) coretyped.PodInterface {
	return &blockedDiscoveryPods{PodInterface: c.CoreV1Interface.Pods(ns), client: c.client, namespace: ns}
}
func (c *blockedDiscoveryCore) Services(ns string) coretyped.ServiceInterface {
	return &blockedDiscoveryServices{ServiceInterface: c.CoreV1Interface.Services(ns), client: c.client, namespace: ns}
}

type blockedDiscoveryPods struct {
	coretyped.PodInterface
	client    *blockedDiscoveryClient
	namespace string
}

func (p *blockedDiscoveryPods) List(ctx context.Context, opts metav1.ListOptions) (*v1.PodList, error) {
	if p.namespace == "test-ns" && p.client.resource == "pods" {
		if err := p.client.block(ctx); err != nil {
			return nil, err
		}
	}
	return p.PodInterface.List(ctx, opts)
}

type blockedDiscoveryServices struct {
	coretyped.ServiceInterface
	client    *blockedDiscoveryClient
	namespace string
}

func (s *blockedDiscoveryServices) List(ctx context.Context, opts metav1.ListOptions) (*v1.ServiceList, error) {
	if s.namespace == "test-ns" && s.client.resource == "services" && (!s.client.cacheOnly || opts.Limit != 1) {
		if err := s.client.block(ctx); err != nil {
			return nil, err
		}
	}
	return s.ServiceInterface.List(ctx, opts)
}
func blockDiscovery(t *testing.T, env *testEnv, resource string, cacheOnly bool) *blockedDiscoveryClient {
	t.Helper()
	blocked := &blockedDiscoveryClient{Interface: env.server.clientset, resource: resource, cacheOnly: cacheOnly, entered: make(chan context.Context, 1), release: make(chan struct{}), finished: make(chan struct{})}
	env.server.routes.clientset = blocked
	t.Cleanup(blocked.open)
	return blocked
}

func awaitDiscoveryState(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-timer.C:
			t.Fatal("discovery lifecycle did not reach expected state")
		case <-ticker.C:
		}
	}
}

func TestIntegration_RouteDiscoverySharedLifecycle(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	blocked := blockDiscovery(t, env, "pods", false)
	aCtx, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	bCtx, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	a, err := env.client.WatchNamespaceRoutes(aCtx, &routeRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initialization didn't start")
	}
	b, err := env.client.WatchNamespaceRoutes(bCtx, &routeRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	broadcaster := env.server.routes
	var hub *nsRouteHub
	awaitDiscoveryState(t, func() bool {
		broadcaster.mu.Lock()
		defer broadcaster.mu.Unlock()
		hub = broadcaster.hubs["test-ns"]
		return hub != nil && hub.refs == 2
	})
	cancelA()
	if result := awaitRouteReceive(t, receiveRouteAsync(a), time.Second); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("A cancel: %+v", result)
	}
	awaitDiscoveryState(t, func() bool { broadcaster.mu.Lock(); defer broadcaster.mu.Unlock(); return hub.refs == 1 })
	if hub.ctx.Err() != nil {
		t.Fatal("canceling first subscriber canceled shared initialization")
	}
	blocked.open()
	frame := recvRoutes(t, b)
	if !frame.Snapshot || !frame.Enabled {
		t.Fatalf("B snapshot: %+v", frame)
	}
	if _, err := env.server.clientset.CoreV1().Services("test-ns").Create(bCtx, mkSvc("later", "10.96.0.20", ""), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	delta := recvRoutes(t, b)
	if delta.Snapshot || len(delta.UpsertedServices) != 1 {
		t.Fatalf("B delta: %+v", delta)
	}
	// A second active subscriber must survive B leaving just as B survived A
	// leaving during initialization. It receives a complete snapshot first.
	cCtx, cancelC := context.WithCancel(context.Background())
	defer cancelC()
	c, err := env.client.WatchNamespaceRoutes(cCtx, &routeRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	if snap := recvRoutes(t, c); len(snap.UpsertedServices) != 1 {
		t.Fatalf("C snapshot: %+v", snap)
	}
	cancelB()
	awaitDiscoveryState(t, func() bool { broadcaster.mu.Lock(); defer broadcaster.mu.Unlock(); return hub.refs == 1 })
	if err := env.server.clientset.CoreV1().Services("test-ns").Delete(cCtx, "later", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if removed := recvRoutes(t, c); !hasStr(removed.RemovedServiceKeys, "test-ns/later") {
		t.Fatalf("C removal: %+v", removed)
	}
	cancelC()
	awaitDiscoveryState(t, func() bool {
		broadcaster.mu.Lock()
		defer broadcaster.mu.Unlock()
		return broadcaster.hubs["test-ns"] == nil && hub.ctx.Err() != nil
	})
	awaitDiscoveryState(t, func() bool { return hub.podInformer.IsStopped() && hub.svcInformer.IsStopped() })
	// No stale generation may poison a later subscription.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconnected, err := env.client.WatchNamespaceRoutes(ctx, &routeRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	if snap := recvRoutes(t, reconnected); !snap.Enabled || len(snap.UpsertedServices) != 0 {
		t.Fatalf("reconnect snapshot: %+v", snap)
	}
	broadcaster.mu.Lock()
	fresh := broadcaster.hubs["test-ns"]
	broadcaster.mu.Unlock()
	if fresh == hub {
		t.Fatal("reconnect reused stopped hub")
	}
}

func TestIntegration_RouteDiscoveryLastWaiterAndServerCancel(t *testing.T) {
	t.Parallel()
	for _, serverCancel := range []bool{false, true} {
		name := "last-waiter"
		if serverCancel {
			name = "server"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			serverCtx, stopServer := context.WithCancel(context.Background())
			defer stopServer()
			env.server.routes.Close()
			env.server.routes = New(serverCtx, env.server.clientset)
			blocked := blockDiscovery(t, env, "pods", false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := env.client.WatchNamespaceRoutes(ctx, &routeRequest{Namespace: "test-ns"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-blocked.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("probe didn't start")
			}
			if serverCancel {
				stopServer()
			} else {
				cancel()
			}
			select {
			case <-blocked.finished:
			case <-time.After(time.Second):
				t.Fatal("cancellation left API probe running")
			}
			if r := awaitRouteReceive(t, receiveRouteAsync(stream), time.Second); !errors.Is(r.err, context.Canceled) {
				t.Fatalf("cancel response: %+v", r)
			}
			awaitDiscoveryState(t, func() bool {
				env.server.routes.mu.Lock()
				defer env.server.routes.mu.Unlock()
				return len(env.server.routes.hubs) == 0
			})
		})
	}
}
