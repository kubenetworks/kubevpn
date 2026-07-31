package action

import (
	"context"
	"sync/atomic"
	"testing"

	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// recordingConn wraps a real ConnectOptions (satisfying handler.Connection) and records
// whether LeaveAllProxyResources was invoked. It lets us assert that disconnect/quit ask
// the manager to remove proxy sidecars/rules while the VPN is still up — i.e. before the
// sudo daemon drops the TUN. Server-side leave reaches the manager over the VPN, so a
// regression that tore the VPN down first would leak the sidecars.
type recordingConn struct {
	*handler.ConnectOptions
	left *atomic.Bool
}

func (r *recordingConn) LeaveAllProxyResources(_ context.Context) error {
	r.left.Store(true)
	return nil
}

var _ handler.Connection = (*recordingConn)(nil)

func newRecordingConn(id string) (*recordingConn, *atomic.Bool) {
	var b atomic.Bool
	return &recordingConn{ConnectOptions: &handler.ConnectOptions{ConnectionID: id}, left: &b}, &b
}

// TestLeaveProxiesBeforeTeardown_All: a disconnect-all request leaves proxy resources on
// every connection before any data-plane teardown.
func TestLeaveProxiesBeforeTeardown_All(t *testing.T) {
	c1, left1 := newRecordingConn("id1")
	c2, left2 := newRecordingConn("id2")
	svr := &Server{connections: []handler.Connection{c1, c2}}

	svr.leaveProxiesBeforeTeardown(context.Background(), &rpc.DisconnectRequest{All: ptr.To(true)})

	if !left1.Load() || !left2.Load() {
		t.Fatalf("expected leave-all on all connections, got id1=%v id2=%v", left1.Load(), left2.Load())
	}
}

// TestLeaveProxiesBeforeTeardown_ByConnectionID: a targeted disconnect leaves only the
// matching connection's proxy resources.
func TestLeaveProxiesBeforeTeardown_ByConnectionID(t *testing.T) {
	c1, left1 := newRecordingConn("id1")
	c2, left2 := newRecordingConn("id2")
	svr := &Server{connections: []handler.Connection{c1, c2}}

	svr.leaveProxiesBeforeTeardown(context.Background(), &rpc.DisconnectRequest{ConnectionID: ptr.To("id2")})

	if left1.Load() {
		t.Error("did not expect leave on the non-targeted connection id1")
	}
	if !left2.Load() {
		t.Error("expected leave on the targeted connection id2")
	}
}

// TestLeaveProxiesBeforeTeardown_NoMatch: a request that matches no connection leaves
// nothing (and does not panic).
func TestLeaveProxiesBeforeTeardown_NoMatch(t *testing.T) {
	c1, left1 := newRecordingConn("id1")
	svr := &Server{connections: []handler.Connection{c1}}

	svr.leaveProxiesBeforeTeardown(context.Background(), &rpc.DisconnectRequest{ConnectionID: ptr.To("missing")})

	if left1.Load() {
		t.Fatal("expected no leave when the connection ID does not match")
	}
}

// TestLeaveProxiesBeforeTeardown_SkipsDataPlaneOnlyConnections exercises the Interface
// Segregation split end-to-end through the real disconnect code path: a *DataSession
// (DataPlane, NOT ProxyController) in the connection slice must be silently skipped —
// the type assertion to ProxyController fails, so LeaveAllProxyResources is never
// called and nothing panics. This is the data-plane half of the contract that
// recordingConn (ProxyController) covers for the control plane.
func TestLeaveProxiesBeforeTeardown_SkipsDataPlaneOnlyConnections(t *testing.T) {
	ctrl, leftCtrl := newRecordingConn("ctrl-id") // *ConnectOptions via embed → ProxyController
	ds := &handler.DataSession{ConnectionID: "data-id"} // DataPlane, NOT ProxyController
	svr := &Server{connections: []handler.Connection{ctrl, ds}}

	// Must not panic on the DataSession even though it lacks LeaveAllProxyResources.
	svr.leaveProxiesBeforeTeardown(context.Background(), &rpc.DisconnectRequest{All: ptr.To(true)})

	if !leftCtrl.Load() {
		t.Error("expected leave on the control-plane (ProxyController) connection")
	}
	// The DataSession has no LeaveAllProxyResources to record; the only assertion
	// reachable here is that we got here without panicking, which is the contract:
	// a data-plane-only connection is a valid member of []Connection.
}

// TestRoleInterfaces_SegregationContract is a compile-time + behavioral integration
// assertion of the dual-session Interface Segregation: *ConnectOptions satisfies
// ProxyController (not DataPlane) and *DataSession satisfies DataPlane (not
// ProxyController), while both satisfy the shared Connection interface. A mixed
// []Connection slice (one of each plane) sorts without panic via sort.Connects,
// whose DataPlane assertions fall back to zero on the control-plane member.
func TestRoleInterfaces_SegregationContract(t *testing.T) {
	var (
		_ handler.Connection      = (*handler.ConnectOptions)(nil)
		_ handler.ProxyController = (*handler.ConnectOptions)(nil)

		_ handler.Connection = (*handler.DataSession)(nil)
		_ handler.DataPlane  = (*handler.DataSession)(nil)
	)

	// Mixed-plane slice, as the root daemon never holds but the disconnect loop
	// must tolerate (and sort.Connects must order without panicking). The control-
	// plane member is not DataPlane, so GetAPIServerIPs/GetNetworkExtraHost fall back
	// to nil inside Less — no panic, no dependency edge detected (correct for a
	// control-plane connection that owns no TUN).
	mixed := handler.Connects{
		&handler.DataSession{ConnectionID: "data"},
		&handler.ConnectOptions{ConnectionID: "ctrl"},
	}
	sorted := mixed.Sort()
	if len(sorted) != 2 {
		t.Fatalf("expected 2 connections after sort, got %d", len(sorted))
	}
}
