package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/network"
)

// Status reads copy presentation data while the session retains the informer and sync resources.
func TestSessionStatus_CacheAndSyncSnapshot(t *testing.T) {
	const envoy = `[{"UID":"deployments.apps.web","Namespace":"work","Rules":[{"OwnerID":"alice"}]}]`
	client := fake.NewSimpleClientset(&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "manager"}, Data: map[string]string{config.KeyEnvoy: envoy}})
	control := &ConnectOptions{ManagerNamespace: "manager", SessionBase: SessionBase{K8sClient: K8sClient{clientset: client}}}
	defer control.Cleanup(context.Background())
	control.SetSync(&SyncOptions{Workloads: []string{"deploy/web"}, Headers: map[string]string{"user": "alice"}, TargetWorkloadNames: map[string]string{"deploy/web": "deploy/web-sync"}})
	if err := control.EnsureConfigMapSynced(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := control.StatusSnapshot(context.Background())
	if err != nil || snapshot.Envoy != envoy || snapshot.Sync == nil {
		t.Fatalf("status: %+v, %v", snapshot, err)
	}
	snapshot.Sync.Workloads[0] = "changed"
	snapshot.Sync.Headers["user"] = "bob"
	snapshot.Sync.TargetWorkloadNames["deploy/web"] = "changed"
	again, err := control.StatusSnapshot(context.Background())
	if err != nil || again.Sync.Workloads[0] != "deploy/web" || again.Sync.Headers["user"] != "alice" || again.Sync.TargetWorkloadNames["deploy/web"] != "deploy/web-sync" {
		t.Fatal("status snapshot exposes mutable sync state")
	}
	data := &DataSession{ManagerNamespace: "manager", SessionBase: SessionBase{K8sClient: K8sClient{clientset: client}}}
	defer data.Cleanup(context.Background())
	if err := data.getConfigMapStore().EnsureSynced(context.Background()); err != nil {
		t.Fatal(err)
	}
	root, err := data.StatusSnapshot(context.Background())
	if err != nil || root.Envoy != envoy || root.Sync != nil {
		t.Fatalf("root status: %+v, %v", root, err)
	}
}

// Node discovery updates runtime-owned routes without losing session status metadata.
func TestC2A_RuntimeNodeRoutesRemainVisible(t *testing.T) {
	client := fake.NewSimpleClientset(&v1.Node{Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "10.20.30.40"},
	}}})
	ds := &DataSession{ExtraRouteInfo: ExtraRouteInfo{ExtraCIDR: []string{"10.96.0.0/12"}, ExtraNodeIP: true}}
	ds.nm = network.New(network.Config{Clientset: client, RouteConfig: &network.RouteConfig{
		ExtraCIDR: append([]string(nil), ds.ExtraRouteInfo.ExtraCIDR...), ExtraNodeIP: ds.ExtraRouteInfo.ExtraNodeIP,
	}})
	defer ds.Cleanup(context.Background())
	if err := ds.nm.AddExtraNodeIP(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := ds.GetExtraCIDR()
	if len(got) != 2 || got[0] != "10.96.0.0/12" || got[1] != "10.20.30.40/32" {
		t.Fatalf("runtime routes missing from session status: %v", got)
	}
	if len(ds.ExtraRouteInfo.ExtraCIDR) != 1 {
		t.Fatal("runtime mutated handler configuration")
	}
}

// TestC2A_RuntimeFailureCleanup uses the public runtime boundary with a real
// Kubernetes HTTP transport, then joins cleanup through the data-plane session.
func TestC2A_RuntimeFailureCleanup(t *testing.T) {
	t.Setenv("KUBECTL_REMOTE_COMMAND_WEBSOCKETS", "false")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":403,"message":"denied"}`)
	}))
	defer server.Close()
	cfg := &rest.Config{Host: server.URL, APIPath: "/api", ContentConfig: rest.ContentConfig{
		GroupVersion:         &schema.GroupVersion{Version: "v1"},
		NegotiatedSerializer: serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion(),
	}}
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ds := &DataSession{nm: network.New(network.Config{
		Config: cfg, RESTClient: client, ManagerNamespace: "test-ns",
		GetRunningPodList: func(context.Context) ([]v1.Pod, error) { return []v1.Pod{{}}, nil },
	})}
	ds.ctx, ds.cancel = context.WithCancel(context.Background())
	defer ds.Cleanup(context.Background())
	result := make(chan error, 1)
	go func() { result <- ds.nm.Start(ds.ctx) }()
	select {
	case err = <-result:
		if err == nil || ds.nm.Err() == nil {
			t.Fatal("terminal authorization failure was lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed Start self-joined")
	}
	select {
	case <-ds.nm.Done():
	default:
		t.Fatal("terminal runtime did not signal completion")
	}
	rolledBack := 0
	ds.AddRollbackFunc(func() error { rolledBack++; return nil })
	ds.Cleanup(context.Background())
	ds.Cleanup(context.Background())
	if ds.Context().Err() == nil || rolledBack != 1 {
		t.Fatalf("session cleanup: context=%v rollbacks=%d", ds.Context().Err(), rolledBack)
	}
	if err := ds.nm.Start(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stopped runtime accepted restart: %v", err)
	}
}

// TestC2A_ControlSession_SatisfiesProxyController verifies the Interface Segregation
// contract for the user-daemon session: *ConnectOptions satisfies the shared
// Connection interface AND the control-plane role interface ProxyController, and
// the shared accessors return safe zero values on a zero-value construction.
// It deliberately does NOT satisfy DataPlane — the data-plane methods (DoConnect,
// GetLocalTunIP, GetAPIServerIPs, GetNetworkExtraHost, GetLastHeartbeat) are absent
// from ConnectOptions, which is the whole point of the split: a wrong-plane call
// now fails to compile instead of returning a stub error at runtime.
func TestC2A_ControlSession_SatisfiesProxyController(t *testing.T) {
	c := &ConnectOptions{} // = &ControlSession{}

	// Compile-time role assertions (also enforced in connection.go; repeated here
	// so a future regression that re-adds a data-plane stub to ConnectOptions is
	// caught by this test failing to express the intent).
	var _ Connection = c
	var _ ProxyController = c

	// Shared accessors return safe zero values.
	if ctx := c.Context(); ctx != nil {
		t.Errorf("expected nil Context() on ControlSession, got %v", ctx)
	}
	if s := c.GetSync(); s != nil {
		t.Errorf("expected nil GetSync on ControlSession, got %v", s)
	}
	if addr := c.GetSocksListenAddr(); addr != "" {
		t.Errorf("expected empty SocksListenAddr on ControlSession, got %s", addr)
	}
	if c.GetSocksEgress() {
		t.Error("expected false SocksEgress on ControlSession")
	}

	// Cleanup on zero-value ControlSession must not panic.
	// cleanupControlPlane is called; clientset is nil so the Delete calls are skipped,
	// LeaveAllProxyResources returns nil immediately (proxyManager is nil).
	c.Cleanup(context.Background())
}

// TestC2A_DataSession_SatisfiesDataPlane verifies the Interface Segregation contract
// for the root-daemon session: *DataSession satisfies Connection AND the data-plane
// role interface DataPlane, and the shared GetSync accessor returns nil (file sync is
// a control-plane responsibility). It deliberately does NOT satisfy ProxyController —
// the proxy methods (CreateRemoteInboundPod, LeaveAllProxyResources, LeaveResource,
// ProxyResources, SetSync) are absent from DataSession.
func TestC2A_DataSession_SatisfiesDataPlane(t *testing.T) {
	ds := &DataSession{}

	var _ Connection = ds
	var _ DataPlane = ds

	if _, ok := any(ds).(ProxyController); ok {
		t.Fatal("data session must not expose control-plane resources")
	}
}

// TestC2A_Cleanup_ControlPlanePath verifies that Cleanup on a zero-value ConnectOptions
// (user-daemon path) is idempotent and does not panic.
func TestC2A_Cleanup_ControlPlanePath(t *testing.T) {
	c := &ConnectOptions{}
	// First call: cleanupControlPlane path (no clientset, no proxyManager).
	c.Cleanup(context.Background())
	// Second call must be a no-op (cleanedUp flag prevents re-entry).
	c.Cleanup(context.Background())
}

// TestC2A_Cleanup_DataPlanePath verifies that Cleanup on a DataSession with rollback functions
// calls those functions and marks the connection as cleaned up.
func TestC2A_Cleanup_DataPlanePath(t *testing.T) {
	rollbackCalled := false
	ds := &DataSession{}
	ds.AddRollbackFunc(func() error {
		rollbackCalled = true
		return nil
	})

	// First Cleanup: data-plane path.
	ds.Cleanup(context.Background())

	if !rollbackCalled {
		t.Error("expected rollback function to be called in DataSession cleanup")
	}

	// Second call must be a no-op (cleanedUp flag).
	rollbackCalled = false
	ds.Cleanup(context.Background())
	if rollbackCalled {
		t.Error("rollback function must not be called again after cleanedUp is set")
	}
}

// TestC2A_TypeAlias_ControlSessionEqualsConnectOptions verifies that ControlSession is a true
// type alias (not a type definition): both pointer types are identical and assignable.
func TestC2A_TypeAlias_ControlSessionEqualsConnectOptions(t *testing.T) {
	var cs *ControlSession = &ConnectOptions{}
	var co *ConnectOptions = cs
	_ = co
	// If this compiles, the alias is correct. Both are the same type.
}

// TestC2A_DataSession_GetConnectionID verifies nil-safe receiver behavior.
func TestC2A_DataSession_GetConnectionID(t *testing.T) {
	var ds *DataSession
	if id := ds.GetConnectionID(); id != "" {
		t.Fatalf("expected empty ID on nil DataSession, got %q", id)
	}
	ds = &DataSession{ConnectionID: "abc123def456"}
	if id := ds.GetConnectionID(); id != "abc123def456" {
		t.Fatalf("expected abc123def456, got %q", id)
	}
}

// TestC2A_DataSession_GetOwnerID verifies nil-safe receiver behavior.
func TestC2A_DataSession_GetOwnerID(t *testing.T) {
	var ds *DataSession
	if id := ds.GetOwnerID(); id != "" {
		t.Fatalf("expected empty ID on nil DataSession, got %q", id)
	}
	ds = &DataSession{OwnerID: "owner-id-1"}
	if id := ds.GetOwnerID(); id != "owner-id-1" {
		t.Fatalf("expected owner-id-1, got %q", id)
	}
}

// TestC2A_DataSession_NilNmGuards verifies that data-plane accessors guard ds.nm == nil.
func TestC2A_DataSession_NilNmGuards(t *testing.T) {
	ds := &DataSession{} // nm is nil

	// GetLocalTunIP: both empty.
	v4, v6 := ds.GetLocalTunIP()
	if v4 != "" || v6 != "" {
		t.Errorf("expected empty TUN IPs when nm is nil, got %q/%q", v4, v6)
	}

	// GetLastHeartbeat: zero time.
	if hb := ds.GetLastHeartbeat(); !hb.IsZero() {
		t.Errorf("expected zero heartbeat when nm is nil, got %v", hb)
	}

	// GetAPIServerIPs: nil.
	if ips := ds.GetAPIServerIPs(); ips != nil {
		t.Errorf("expected nil APIServerIPs when nm is nil, got %v", ips)
	}

	// GetNetworkExtraHost: nil.
	if hosts := ds.GetNetworkExtraHost(); hosts != nil {
		t.Errorf("expected nil NetworkExtraHost when nm is nil, got %v", hosts)
	}

	// Context: nil (DoConnect not called).
	if ctx := ds.Context(); ctx != nil {
		t.Errorf("expected nil Context before DoConnect, got %v", ctx)
	}
}

// TestC2A_SessionBase_AddRollbackFunc verifies that AddRollbackFunc is promoted to both types.
func TestC2A_SessionBase_AddRollbackFunc(t *testing.T) {
	t.Run("ConnectOptions", func(t *testing.T) {
		c := &ConnectOptions{}
		var order []int
		c.AddRollbackFunc(func() error { order = append(order, 1); return nil })
		c.AddRollbackFunc(func() error { order = append(order, 2); return nil })
		funcs := c.getRollbackFuncs()
		if len(funcs) != 2 {
			t.Fatalf("expected 2, got %d", len(funcs))
		}
	})
	t.Run("DataSession", func(t *testing.T) {
		ds := &DataSession{}
		var order []int
		ds.AddRollbackFunc(func() error { order = append(order, 1); return nil })
		ds.AddRollbackFunc(func() error { order = append(order, 2); return nil })
		funcs := ds.getRollbackFuncs()
		if len(funcs) != 2 {
			t.Fatalf("expected 2, got %d", len(funcs))
		}
	})
}
