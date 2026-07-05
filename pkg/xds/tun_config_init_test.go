package xds

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// setFastInitBackoff shrinks initDHCPBackoff for the duration of a test so the
// retry loop runs in milliseconds. Not parallel-safe (mutates a package var) —
// these tests must not call t.Parallel().
func setFastInitBackoff(t *testing.T, steps int) {
	t.Helper()
	orig := initDHCPBackoff
	initDHCPBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 1.0, Steps: steps}
	t.Cleanup(func() { initDHCPBackoff = orig })
}

// serveTunConfigOverGRPC stands up a real gRPC server for s and returns a
// connected client, mirroring the production wiring (register + serve + dial).
func serveTunConfigOverGRPC(t *testing.T, s *TunConfigServer) rpc.TunConfigServiceClient {
	t.Helper()
	grpcServer := grpc.NewServer()
	rpc.RegisterTunConfigServiceServer(grpcServer, s)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go grpcServer.Serve(lis)

	conn, err := grpc.DialContext(context.Background(), lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		grpcServer.Stop()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		grpcServer.Stop()
	})
	return rpc.NewTunConfigServiceClient(conn)
}

// connRefusedClientset returns a fake clientset whose ConfigMap "get" fails with a
// connection-refused-style error for the first failCount calls, then behaves
// normally. This reproduces the CI failure where a fresh traffic-manager pod's xds
// container reaches the K8s API before the pod network is ready.
func connRefusedClientset(failCount int32, objects ...runtime.Object) *fake.Clientset {
	clientset := fake.NewSimpleClientset(objects...)
	var calls int32
	clientset.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&calls, 1) <= failCount {
			return true, nil, fmt.Errorf(`Get "https://10.96.0.1:443/api/v1/namespaces/kubevpn/configmaps/%s": dial tcp 10.96.0.1:443: connect: connection refused`, config.ConfigMapPodTrafficManager)
		}
		return false, nil, nil // fall through to the default tracker
	})
	return clientset
}

func trafficManagerObjects() []runtime.Object {
	return []runtime.Object{
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubevpn", UID: "uid-central"}},
		&v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
			Data: map[string]string{
				config.KeyTunIPPool: "",
				config.KeyEnvoy:     "",
			},
		},
	}
}

// TestInit_TransientAPIFailure_RecoversAndServes reproduces the central-mode CI
// regression: the API server is briefly unreachable at xds startup. With retry,
// NewTunConfigServer must eventually succeed, and the resulting server must serve
// TunConfigService so a client can allocate a TUN IP end-to-end over gRPC.
func TestInit_TransientAPIFailure_RecoversAndServes(t *testing.T) {
	setFastInitBackoff(t, 8)

	// Fail the first 2 ConfigMap gets, then let InitDHCP succeed.
	clientset := connRefusedClientset(2, trafficManagerObjects()...)

	s, err := NewTunConfigServer(context.Background(), clientset, "kubevpn")
	if err != nil {
		t.Fatalf("NewTunConfigServer should recover after transient failures, got: %v", err)
	}

	// Prove the data plane actually works: register + serve + allocate over gRPC.
	client := serveTunConfigOverGRPC(t, s)
	resp, err := client.GetTunIP(context.Background(), &rpc.TunIPRequest{
		OwnerID:   "central-client",
		Namespace: "kubevpn",
	})
	if err != nil {
		t.Fatalf("GetTunIP over gRPC: %v", err)
	}
	ip, _, err := net.ParseCIDR(resp.IPv4)
	if err != nil {
		t.Fatalf("parse allocated IP %q: %v", resp.IPv4, err)
	}
	if !config.CIDR.Contains(ip) {
		t.Fatalf("allocated IP %s not in CIDR %s", ip, config.CIDR)
	}
}

// TestInit_PermanentAPIFailure_IsFatal asserts that when InitDHCP never succeeds,
// NewTunConfigServer returns an error (surfacing the real cause) rather than a nil
// server. Main relies on this to exit fatally so K8s restarts the container,
// instead of serving an xDS endpoint without TunConfigService.
func TestInit_PermanentAPIFailure_IsFatal(t *testing.T) {
	setFastInitBackoff(t, 3)

	// A very large fail budget => every attempt fails.
	clientset := connRefusedClientset(1<<30, trafficManagerObjects()...)

	s, err := NewTunConfigServer(context.Background(), clientset, "kubevpn")
	if err == nil {
		t.Fatal("NewTunConfigServer should fail when InitDHCP never succeeds")
	}
	if s != nil {
		t.Fatalf("expected nil server on permanent failure, got %v", s)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error should surface the underlying InitDHCP cause, got: %v", err)
	}
}
