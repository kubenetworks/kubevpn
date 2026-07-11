package handler

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/xds"
)

// TestIntegration_ServiceIPRouted_SameNamespaceAsManager wires the real server-side route
// discovery (TunConfigServer) over a real gRPC loopback to the real client apply path
// (applyRouteFrame), reproducing the reported bug: a Service living in the SAME namespace as
// the traffic manager must have its ClusterIP added to the route table, not merely fed to DNS.
//
// Before the fix, applyRouteFrame routed only pod CIDRs and dropped service ClusterIPs on the
// assumption that the whole service CIDR was routed at connect. When service-CIDR detection is
// incomplete/filtered, the ClusterIP resolves (hosts entry) but has no route — "connects" fail.
func TestIntegration_ServiceIPRouted_SameNamespaceAsManager(t *testing.T) {
	ctx := context.Background()
	// Manager namespace == workload namespace: exactly the "same ns as traffic manager" case.
	const ns = "kubevpn"

	clientset := fake.NewSimpleClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: "uid-ns-1"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: ns},
			Data:       map[string]string{config.KeyTunIPPool: "", config.KeyEnvoy: ""},
		},
		// A normal ClusterIP service in the manager namespace — the one that was unreachable.
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
			Spec:       v1.ServiceSpec{ClusterIP: "172.21.0.55", ClusterIPs: []string{"172.21.0.55"}},
		},
		// Headless service contributes no routable IP.
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "headless", Namespace: ns},
			Spec:       v1.ServiceSpec{ClusterIP: v1.ClusterIPNone},
		},
		// ExternalName service is a DNS CNAME with no ClusterIP — must not be routed.
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: ns},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeExternalName, ExternalName: "example.com"},
		},
	)

	server, err := xds.NewTunConfigServer(ctx, clientset, ns)
	if err != nil {
		t.Fatalf("NewTunConfigServer: %v", err)
	}
	grpcServer := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(grpcServer, server)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.DialContext(ctx,
		fmt.Sprintf("127.0.0.1:%d", lis.Addr().(*net.TCPAddr).Port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := rpc.NewTunConfigServiceClient(conn).WatchNamespaceRoutes(streamCtx,
		&rpc.NamespaceRoutesRequest{Namespace: ns})
	if err != nil {
		t.Fatalf("WatchNamespaceRoutes: %v", err)
	}

	// Receive the initial snapshot frame (with a timeout so a hang fails the test).
	type recvResult struct {
		resp *rpc.NamespaceRoutesResponse
		err  error
	}
	rc := make(chan recvResult, 1)
	go func() { r, e := stream.Recv(); rc <- recvResult{r, e} }()
	var snap *rpc.NamespaceRoutesResponse
	select {
	case r := <-rc:
		if r.err != nil {
			t.Fatalf("Recv snapshot: %v", r.err)
		}
		snap = r.resp
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for snapshot frame")
	}
	if !snap.Snapshot || !snap.Enabled {
		t.Fatalf("first frame Snapshot=%v Enabled=%v; want both true", snap.Snapshot, snap.Enabled)
	}

	// Feed the real server frame through the real client apply path, capturing routed IPs.
	routed := map[string]bool{}
	services := map[string]*rpc.ServiceRecord{}
	applyRouteFrame(snap, services,
		func([]string) {}, // pod CIDR routes — not under test here
		func(ips []string) {
			for _, ip := range ips {
				routed[ip] = true
			}
		},
		func([]v1.Service) {}, // DNS — not under test here
	)

	// The manager-namespace service's ClusterIP must be routed (the bug: it was not).
	if !routed["172.21.0.55"] {
		t.Fatalf("service ClusterIP 172.21.0.55 was not routed; routed=%v", routed)
	}
	// Headless (ClusterIP None) and ExternalName services contribute no routable IP.
	if routed["None"] || routed[""] {
		t.Fatalf("headless/externalName service must not be routed; routed=%v", routed)
	}
	if len(routed) != 1 {
		t.Fatalf("exactly one service IP should be routed, got %v", routed)
	}
}
