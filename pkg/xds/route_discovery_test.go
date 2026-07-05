package xds

import (
	"context"
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding/gzip"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

func TestPodIPToPrefix(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"10.244.1.5", "10.244.1.0/24"},
		{"10.244.1.250", "10.244.1.0/24"},
		{"10.244.2.3", "10.244.2.0/24"},
		{"172.16.5.9", "172.16.5.0/24"},
		{"fd00:10:244::5", "fd00:10:244::/64"},
	}
	for _, c := range cases {
		got, ok := podIPToPrefix(net.ParseIP(c.ip))
		if !ok || got != c.want {
			t.Errorf("podIPToPrefix(%s) = %q,%v; want %q", c.ip, got, ok, c.want)
		}
	}
	if _, ok := podIPToPrefix(nil); ok {
		t.Errorf("podIPToPrefix(nil) should be !ok")
	}
	// Aggressive aggregation must never widen beyond /24 (v4) / /64 (v6).
	if got, _ := podIPToPrefix(net.ParseIP("10.244.1.5")); got == "10.244.0.0/16" || got == "10.0.0.0/8" {
		t.Errorf("prefix over-aggregated: %s", got)
	}
}

func mkPod(name, ip string, hostNet bool) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Spec:       v1.PodSpec{HostNetwork: hostNet},
		Status:     v1.PodStatus{PodIP: ip, PodIPs: []v1.PodIP{{IP: ip}}},
	}
}

func mkSvc(name, clusterIP, externalName string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Spec:       v1.ServiceSpec{ClusterIP: clusterIP, ExternalName: externalName},
	}
}

// recvRoutes reads one frame with a timeout.
func recvRoutes(t *testing.T, stream rpc.TunConfigService_WatchNamespaceRoutesClient) *rpc.NamespaceRoutesResponse {
	t.Helper()
	type res struct {
		resp *rpc.NamespaceRoutesResponse
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := stream.Recv()
		ch <- res{r, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Recv: %v", r.err)
		}
		return r.resp
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for route frame")
		return nil
	}
}

func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestIntegration_WatchNamespaceRoutes_SnapshotAndDelta wires the real TunConfigServer +
// gRPC and drives the full lifecycle: snapshot on subscribe, aggregated pod /24 prefixes,
// service records, and delta frames on pod add / service delete. gzip is enabled on the call.
func TestIntegration_WatchNamespaceRoutes_SnapshotAndDelta(t *testing.T) {
	env := newTestEnv(t)
	cs := env.server.clientset
	ctx := context.Background()

	// Seed pods (two in one /24, one in another; one host-network to be excluded) + services.
	for _, p := range []*v1.Pod{
		mkPod("p1", "10.244.1.5", false),
		mkPod("p2", "10.244.1.9", false),
		mkPod("p3", "10.244.2.3", false),
		mkPod("phost", "192.168.0.5", true),
	} {
		if _, err := cs.CoreV1().Pods("test-ns").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []*v1.Service{
		mkSvc("web", "10.96.0.10", ""),
		mkSvc("headless", v1.ClusterIPNone, ""),
	} {
		if _, err := cs.CoreV1().Services("test-ns").Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A pod in another namespace must NOT leak into test-ns routes.
	_, _ = cs.CoreV1().Pods("other-ns").Create(ctx, &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "other-ns"},
		Status:     v1.PodStatus{PodIP: "10.244.9.9"},
	}, metav1.CreateOptions{})

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := env.client.WatchNamespaceRoutes(streamCtx,
		&rpc.NamespaceRoutesRequest{Namespace: "test-ns"},
		grpc.UseCompressor(gzip.Name),
	)
	if err != nil {
		t.Fatalf("WatchNamespaceRoutes: %v", err)
	}

	// Phase 1: snapshot
	snap := recvRoutes(t, stream)
	if !snap.Snapshot || !snap.Enabled {
		t.Fatalf("first frame Snapshot=%v Enabled=%v; want both true", snap.Snapshot, snap.Enabled)
	}
	if !hasStr(snap.AddedPodCIDRs, "10.244.1.0/24") || !hasStr(snap.AddedPodCIDRs, "10.244.2.0/24") {
		t.Fatalf("snapshot AddedPodCIDRs=%v; want the two /24 prefixes", snap.AddedPodCIDRs)
	}
	if len(snap.AddedPodCIDRs) != 2 {
		t.Fatalf("snapshot AddedPodCIDRs=%v; want exactly 2 (aggregated + no host-net + no cross-ns)", snap.AddedPodCIDRs)
	}
	if hasStr(snap.AddedPodCIDRs, "10.244.9.0/24") {
		t.Fatal("cross-namespace pod leaked into snapshot")
	}
	var web *rpc.ServiceRecord
	for _, s := range snap.UpsertedServices {
		if s.Name == "web" {
			web = s
		}
	}
	if web == nil || len(web.ClusterIPs) != 1 || web.ClusterIPs[0] != "10.96.0.10" {
		t.Fatalf("snapshot missing web service record: %+v", snap.UpsertedServices)
	}

	// Phase 2: add a pod in a new /24 -> delta
	if _, err := cs.CoreV1().Pods("test-ns").Create(ctx, mkPod("p4", "10.244.3.7", false), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	d1 := recvRoutes(t, stream)
	if d1.Snapshot {
		t.Fatal("expected a delta frame, got snapshot")
	}
	if !hasStr(d1.AddedPodCIDRs, "10.244.3.0/24") {
		t.Fatalf("delta AddedPodCIDRs=%v; want 10.244.3.0/24", d1.AddedPodCIDRs)
	}
	if d1.Version <= snap.Version {
		t.Fatalf("delta Version=%d not > snapshot Version=%d", d1.Version, snap.Version)
	}

	// Phase 3: delete a service -> delta with RemovedServiceKeys
	if err := cs.CoreV1().Services("test-ns").Delete(ctx, "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Filter to the frame that carries the service removal (pod churn frames may interleave).
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		d := recvRoutes(t, stream)
		if hasStr(d.RemovedServiceKeys, "test-ns/web") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("did not receive a delta removing service test-ns/web")
	}
}

// TestIntegration_WatchNamespaceRoutes_Degrade verifies that when the manager cannot
// list pods in the namespace (no RBAC), the server serves a single Enabled=false frame
// so the client degrades to CIDR-only routing instead of erroring.
func TestIntegration_WatchNamespaceRoutes_Degrade(t *testing.T) {
	env := newTestEnv(t)
	cs := env.server.clientset.(*fake.Clientset)
	cs.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
	})

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := env.client.WatchNamespaceRoutes(streamCtx, &rpc.NamespaceRoutesRequest{Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("WatchNamespaceRoutes: %v", err)
	}
	frame := recvRoutes(t, stream)
	if frame.Enabled {
		t.Fatalf("expected Enabled=false when pods list is forbidden, got Enabled=true")
	}
}
