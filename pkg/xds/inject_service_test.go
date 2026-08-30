package xds

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// TestProxyInject_DegradesWhenUnavailable verifies the client-fallback contract end
// to end through the real gRPC server + client: when the manager has no factory
// (server-side injection unavailable, e.g. an older manager or missing config), the
// ProxyInject stream fails with codes.Unavailable so the client falls back to local
// injection instead of failing the proxy.
func TestProxyInject_DegradesWhenUnavailable(t *testing.T) {
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
		t.Fatalf("expected codes.Unavailable so the client falls back to local injection, got %v", err)
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

// TestLeaveInject_DegradesWhenUnavailable verifies the same client-fallback contract for
// LeaveInject: no factory -> codes.Unavailable, so the client unpatches locally instead.
func TestLeaveInject_DegradesWhenUnavailable(t *testing.T) {
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
		t.Fatalf("expected codes.Unavailable so the client unpatches locally, got %v", err)
	}
}
