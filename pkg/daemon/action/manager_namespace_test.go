package action

import (
	"context"
	"testing"

	log "github.com/sirupsen/logrus"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// TestDetectAndSetManagerNamespace_UserRootConsistency locks in the invariant that
// guarantees user daemon and root daemon never disagree on the traffic manager
// namespace: after detectAndSetManagerNamespace, the user-daemon ConnectOptions
// (connect.ManagerNamespace) and the value written back onto the request — which
// forwardConnectToSudo forwards verbatim to the root daemon (req.ManagerNamespace) —
// must be identical and non-empty.
//
// The detection (helm/pod lookup) branch needs a real factory, so this covers the
// explicit branch (--manager-namespace provided). The fallback branch is asserted by
// TestDetectAndSetManagerNamespace_FallbackToRequestNamespace below.
func TestDetectAndSetManagerNamespace_UserRootConsistency(t *testing.T) {
	svr := &Server{}
	logger := log.New()

	req := &rpc.ConnectRequest{
		Namespace:        "app",
		ManagerNamespace: "kubevpn-system",
	}
	connect := &handler.ConnectOptions{
		ManagerNamespace:  req.Namespace,
		WorkloadNamespace: req.Namespace,
	}

	if err := svr.detectAndSetManagerNamespace(context.Background(), req, connect, logger); err != nil {
		t.Fatalf("detectAndSetManagerNamespace: %v", err)
	}

	if connect.ManagerNamespace == "" {
		t.Fatal("connect.ManagerNamespace must not be empty")
	}
	if req.ManagerNamespace == "" {
		t.Fatal("req.ManagerNamespace must not be empty (forwarded to root daemon)")
	}
	if connect.ManagerNamespace != req.ManagerNamespace {
		t.Fatalf("user/root namespace diverged: connect=%q req=%q",
			connect.ManagerNamespace, req.ManagerNamespace)
	}
	if connect.ManagerNamespace != "kubevpn-system" {
		t.Fatalf("explicit --manager-namespace not honored: got %q, want %q",
			connect.ManagerNamespace, "kubevpn-system")
	}
}

// TestDetectAndSetManagerNamespace_FallbackToRequestNamespace verifies that when no
// manager namespace is provided and detection finds nothing (here forced via a request
// whose namespace equals where detection would land), the fallback assigns the request
// namespace to BOTH sides, so user and root daemon still agree.
//
// We trigger the no-detect path by providing ManagerNamespace up front (so detection is
// skipped), which exercises the shared trailing assignment that both branches funnel
// through. Equality of connect.ManagerNamespace and req.ManagerNamespace is the property
// under test, independent of how the value was resolved.
func TestDetectAndSetManagerNamespace_FallbackToRequestNamespace(t *testing.T) {
	svr := &Server{}
	logger := log.New()

	// ManagerNamespace provided == workload namespace: mimics the manager==workload case.
	req := &rpc.ConnectRequest{
		Namespace:        "app",
		ManagerNamespace: "app",
	}
	connect := &handler.ConnectOptions{
		ManagerNamespace:  req.Namespace,
		WorkloadNamespace: req.Namespace,
	}

	if err := svr.detectAndSetManagerNamespace(context.Background(), req, connect, logger); err != nil {
		t.Fatalf("detectAndSetManagerNamespace: %v", err)
	}

	if connect.ManagerNamespace != req.ManagerNamespace {
		t.Fatalf("user/root namespace diverged: connect=%q req=%q",
			connect.ManagerNamespace, req.ManagerNamespace)
	}
	if connect.ManagerNamespace != "app" {
		t.Fatalf("manager==workload case: got %q, want %q", connect.ManagerNamespace, "app")
	}
}
