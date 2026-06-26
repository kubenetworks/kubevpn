package handler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestParsePortPair_ColonSeparated(t *testing.T) {
	cases := []struct {
		containerPort int32
		portPair      string
		wantLocal     int32
		wantRemote    int32
		wantOK        bool
	}{
		{8080, "29450:19080", 19080, 29450, true},
		{9090, "30000:9090", 9090, 30000, true},
		{80, "12345:80", 80, 12345, true},
		{8080, "invalid:port", 0, 0, false},
		{8080, "123:invalid", 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.portPair, func(t *testing.T) {
			local, remote, ok := parsePortPair(c.containerPort, c.portPair)
			if ok != c.wantOK {
				t.Fatalf("ok: want %v, got %v", c.wantOK, ok)
			}
			if ok {
				if local != c.wantLocal {
					t.Fatalf("local: want %d, got %d", c.wantLocal, local)
				}
				if remote != c.wantRemote {
					t.Fatalf("remote: want %d, got %d", c.wantRemote, remote)
				}
			}
		})
	}
}

func TestParsePortPair_PlainNumber(t *testing.T) {
	local, remote, ok := parsePortPair(8080, "9090")
	if !ok {
		t.Fatal("expected ok=true for plain number")
	}
	if local != 8080 {
		t.Fatalf("local: want 8080 (containerPort), got %d", local)
	}
	if remote != 9090 {
		t.Fatalf("remote: want 9090, got %d", remote)
	}
}

func TestParsePortPair_InvalidPlain(t *testing.T) {
	_, _, ok := parsePortPair(8080, "not-a-number")
	if ok {
		t.Fatal("expected ok=false for invalid plain number")
	}
}

func TestProxyList_AddRemove(t *testing.T) {
	var list ProxyList
	list.Add(&Proxy{workload: "deploy/app1", namespace: "default"})
	list.Add(&Proxy{workload: "deploy/app2", namespace: "default"})

	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}

	list.Remove("default", "deploy/app1")
	if len(list) != 1 {
		t.Fatalf("after remove: expected 1, got %d", len(list))
	}
	if list[0].workload != "deploy/app2" {
		t.Fatalf("remaining: want deploy/app2, got %s", list[0].workload)
	}
}

func TestProxyList_ToResources(t *testing.T) {
	list := ProxyList{
		{workload: "deployments.apps/reviews", namespace: "default"},
		{workload: "services/productpage", namespace: "bookinfo"},
	}
	res := list.ToResources()
	if len(res) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(res))
	}
	if res[0].Workload != "deployments.apps/reviews" || res[0].Namespace != "default" {
		t.Fatalf("resource 0 mismatch: %+v", res[0])
	}
}

func TestMapper_Stop_Nil(t *testing.T) {
	var m *Mapper
	// Calling Stop on a nil Mapper must not panic.
	m.Stop()
}

func TestMapper_Stop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mapper{
		ns:       "default",
		workload: "deployments.apps/nginx",
		ctx:      ctx,
		cancel:   cancel,
	}
	// Verify context is not cancelled before Stop.
	if m.ctx.Err() != nil {
		t.Fatal("context should not be cancelled before Stop")
	}
	m.Stop()
	// After Stop, the context must be cancelled.
	if m.ctx.Err() == nil {
		t.Fatal("context should be cancelled after Stop")
	}
}

func TestNewMapper(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ns := "test-ns"
	labels := "app=web"
	headers := map[string]string{"x-env": "dev"}
	workload := "deployments.apps/myapp"

	cmInformer := cache.NewSharedInformer(nil, &v1.ConfigMap{}, 0)

	m := NewMapper(clientset, ns, labels, headers, workload, cmInformer)
	if m == nil {
		t.Fatal("NewMapper returned nil")
	}
	if m.ns != ns {
		t.Fatalf("ns: want %q, got %q", ns, m.ns)
	}
	if m.labels != labels {
		t.Fatalf("labels: want %q, got %q", labels, m.labels)
	}
	if m.workload != workload {
		t.Fatalf("workload: want %q, got %q", workload, m.workload)
	}
	if m.headers["x-env"] != "dev" {
		t.Fatalf("headers mismatch: %v", m.headers)
	}
	if m.ctx == nil {
		t.Fatal("ctx is nil")
	}
	if m.cancel == nil {
		t.Fatal("cancel is nil")
	}
	if m.clientset == nil {
		t.Fatal("clientset is nil")
	}
	if m.cmInformer == nil {
		t.Fatal("cmInformer is nil")
	}
	// Verify the context is live.
	if m.ctx.Err() != nil {
		t.Fatal("context should be active after NewMapper")
	}
}

func TestProxyList_IsMe_Nil(t *testing.T) {
	var list *ProxyList
	// A nil ProxyList must return false, not panic.
	if list.IsMe("default", "deployments.apps.nginx", nil) {
		t.Fatal("nil ProxyList should return false")
	}
}

func TestProxyList_IsMe_NoMatch(t *testing.T) {
	list := &ProxyList{}
	*list = append(*list, &Proxy{
		workload:  "deployments.apps/reviews",
		namespace: "default",
		headers:   map[string]string{"x-env": "prod"},
	})

	// Wrong UID (converts to deployments.apps/nginx, which doesn't match reviews).
	if list.IsMe("default", "deployments.apps.nginx", map[string]string{"x-env": "prod"}) {
		t.Fatal("should not match: wrong workload")
	}
	// Wrong namespace.
	if list.IsMe("other-ns", "deployments.apps.reviews", map[string]string{"x-env": "prod"}) {
		t.Fatal("should not match: wrong namespace")
	}
	// Wrong headers.
	if list.IsMe("default", "deployments.apps.reviews", map[string]string{"x-env": "dev"}) {
		t.Fatal("should not match: wrong headers")
	}
	// Correct match — verify it does match.
	if !list.IsMe("default", "deployments.apps.reviews", map[string]string{"x-env": "prod"}) {
		t.Fatal("should match: all fields correct")
	}
}

func TestCancelAllTunnels(t *testing.T) {
	var called atomic.Int32
	tunnels := &sync.Map{}

	// Store 3 cancel functions.
	for i := 0; i < 3; i++ {
		_, cancel := context.WithCancel(context.Background())
		name := "pod-" + string(rune('a'+i))
		wrappedCancel := func() {
			called.Add(1)
			cancel()
		}
		tunnels.Store(name, context.CancelFunc(wrappedCancel))
	}

	cancelAllTunnels(tunnels)

	// All 3 cancel funcs must have been called.
	if got := called.Load(); got != 3 {
		t.Fatalf("expected 3 cancels called, got %d", got)
	}

	// The map must be empty after cancelAllTunnels.
	count := 0
	tunnels.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("expected empty map after cancelAllTunnels, got %d entries", count)
	}
}

