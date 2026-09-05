package xds

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// TestProxyInject_UnavailableWithoutFactory verifies that when the manager has no factory
// (server-side injection not configured), the ProxyInject stream fails cleanly with
// codes.Unavailable rather than panicking, end to end through the real gRPC server.
func TestProxyInject_UnavailableWithoutFactory(t *testing.T) {
	env := newTestEnv(t) // server.factory is nil (not set by NewTunConfigServer)

	stream, err := env.client.ProxyInject(context.Background(), &rpc.InjectRequest{
		Namespace:    "test-ns",
		Workloads:    []string{"deployments.apps/foo"},
		OwnerID:      "owner-a",
		LocalTunIPv4: "198.18.0.5",
	})
	if err != nil {
		t.Fatalf("ProxyInject open stream: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable when factory unset, got %v", err)
	}
}

// TestProxyInject_RejectsEmptyTunIP verifies input validation: with a factory present
// but no local TUN IPv4, the manager rejects the request with InvalidArgument before
// touching the cluster. The factory points at a nonexistent kubeconfig (never dialed,
// since the validation error fires first) to keep this a no-cluster test.
func TestProxyInject_RejectsEmptyTunIP(t *testing.T) {
	env := newTestEnv(t)
	env.server.factory = util.InitFactoryByPath("/nonexistent-kubeconfig", "test-ns")

	stream, err := env.client.ProxyInject(context.Background(), &rpc.InjectRequest{
		Namespace: "test-ns",
		Workloads: []string{"deployments.apps/foo"},
		OwnerID:   "owner-a",
		// LocalTunIPv4 intentionally empty
	})
	if err != nil {
		t.Fatalf("ProxyInject open stream: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected codes.InvalidArgument for empty TUN IPv4, got %v", err)
	}
}

// TestLeaveInject_UnavailableWithoutFactory verifies LeaveInject fails cleanly with
// codes.Unavailable when the manager has no factory (not configured), via real gRPC.
func TestLeaveInject_UnavailableWithoutFactory(t *testing.T) {
	env := newTestEnv(t) // server.factory is nil

	stream, err := env.client.LeaveInject(context.Background(), &rpc.InjectRequest{
		Namespace: "test-ns",
		Workloads: []string{"deployments.apps/foo"},
		OwnerID:   "owner-a",
	})
	if err != nil {
		t.Fatalf("LeaveInject open stream: %v", err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable when factory unset, got %v", err)
	}
}
