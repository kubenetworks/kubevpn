package xds

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	coretyped "k8s.io/client-go/kubernetes/typed/core/v1"
	clienttesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type routeReceive struct {
	frame *rpc.NamespaceRoutesResponse
	err   error
}

func receiveRouteAsync(stream rpc.TunConfigService_WatchNamespaceRoutesClient) <-chan routeReceive {
	out := make(chan routeReceive, 1)
	go func() { frame, err := stream.Recv(); out <- routeReceive{frame, err} }()
	return out
}

func awaitRouteReceive(t *testing.T, out <-chan routeReceive, timeout time.Duration) routeReceive {
	t.Helper()
	select {
	case result := <-out:
		return result
	case <-time.After(timeout):
		t.Fatal("route initialization did not finish within its budget")
		return routeReceive{}
	}
}

func allocateDuringDiscovery(t *testing.T, env *testEnv, owner string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := env.client.GetTunIP(ctx, &rpc.TunIPRequest{OwnerID: owner, Namespace: "test-ns"})
	if err != nil || resp.GetIPv4() == "" {
		t.Fatalf("GetTunIP failed during discovery: %v %+v", err, resp)
	}
}

func TestIntegration_RouteDiscoveryFailureRecovery(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"pods-forbidden", "services-forbidden", "services-unavailable", "cache-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			cs := env.server.clientset.(*fake.Clientset)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := cs.CoreV1().Services("test-ns").Create(ctx, mkSvc("web", "10.96.0.10", ""), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			var failing atomic.Bool
			failing.Store(true)
			var serviceLists atomic.Int32
			cs.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
				resource := a.GetResource().Resource
				var listNumber int32
				if resource == "services" {
					listNumber = serviceLists.Add(1)
				}
				if !failing.Load() {
					return false, nil, nil
				}
				if failure == "pods-forbidden" && resource == "pods" || failure == "services-forbidden" && resource == "services" {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", fmt.Errorf("injected denial"))
				}
				if resource == "services" && (failure == "services-unavailable" || failure == "cache-unavailable" && listNumber > 1) {
					return true, nil, apierrors.NewServiceUnavailable("injected service API outage")
				}
				return false, nil, nil
			})
			allocateDuringDiscovery(t, env, "before")
			stream, err := env.client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
			if err != nil {
				t.Fatal(err)
			}
			budget := 5 * time.Second
			if failure == "cache-unavailable" {
				budget = 40 * time.Second
			}
			result := awaitRouteReceive(t, receiveRouteAsync(stream), budget)
			switch failure {
			case "pods-forbidden", "services-forbidden":
				if result.err != nil || !result.frame.GetSnapshot() || result.frame.GetEnabled() {
					t.Fatalf("permission denial must disable discovery: %+v", result)
				}
			case "services-unavailable":
				if status.Code(result.err) != codes.Unavailable || result.frame != nil {
					t.Fatalf("API failure must not look like an empty namespace: %+v", result)
				}
			case "cache-unavailable":
				if status.Code(result.err) != codes.DeadlineExceeded || result.frame != nil {
					t.Fatalf("unsynced cache must not produce a snapshot: %+v", result)
				}
			}
			allocateDuringDiscovery(t, env, "during")
			// Repair the API and resubscribe; a failed hub must not poison this namespace.
			failing.Store(false)
			recovered, err := env.client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
			if err != nil {
				t.Fatal(err)
			}
			frame := recvRoutes(t, recovered)
			if !frame.Enabled || !frame.Snapshot || len(frame.UpsertedServices) != 1 || frame.UpsertedServices[0].Name != "web" {
				t.Fatalf("recovery snapshot: %+v", frame)
			}
		})
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

func TestIntegration_RouteDiscoveryInitializationBounded(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"pods-probe", "services-probe", "services-cache"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t)
			resource := "services"
			if stage == "pods-probe" {
				resource = "pods"
			}
			blocked := blockDiscovery(t, env, resource, stage == "services-cache")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := time.Now()
			stream, err := env.client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
			if err != nil {
				t.Fatal(err)
			}
			result := awaitRouteReceive(t, receiveRouteAsync(stream), 40*time.Second)
			if status.Code(result.err) != codes.DeadlineExceeded || result.frame != nil {
				t.Fatalf("initialization should time out without a snapshot: %+v", result)
			}
			if ctx.Err() != nil || time.Since(started) < 29*time.Second {
				t.Fatalf("wrong deadline: parent=%v elapsed=%s", ctx.Err(), time.Since(started))
			}
			select {
			case <-blocked.finished:
			case <-time.After(time.Second):
				t.Fatal("timed-out initialization left an API request running")
			}
			allocateDuringDiscovery(t, env, "after-timeout")
		})
	}
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
	a, err := env.client.WatchNamespaceRoutes(aCtx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initialization didn't start")
	}
	b, err := env.client.WatchNamespaceRoutes(bCtx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
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
	if result := awaitRouteReceive(t, receiveRouteAsync(a), time.Second); status.Code(result.err) != codes.Canceled {
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
	c, err := env.client.WatchNamespaceRoutes(cCtx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
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
	reconnected, err := env.client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
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
			env.server.routes.ctx = serverCtx
			blocked := blockDiscovery(t, env, "pods", false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := env.client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
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
			if r := awaitRouteReceive(t, receiveRouteAsync(stream), time.Second); status.Code(r.err) != codes.Canceled {
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

func TestIntegration_RouteDiscoveryNamespacesIndependent(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	blocked := blockDiscovery(t, env, "pods", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := env.client.WatchNamespaceRoutes(ctx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe not entered")
	}
	allocateDuringDiscovery(t, env, "during-block")
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()
	other, err := env.client.WatchNamespaceRoutes(otherCtx, &rpc.NamespaceRoutesRequest{Namespace: "other-ns"})
	if err != nil {
		t.Fatal(err)
	}
	frame := recvRoutes(t, other)
	if !frame.Enabled || !frame.Snapshot || len(frame.UpsertedServices) != 0 {
		t.Fatalf("healthy empty namespace: %+v", frame)
	}
}
