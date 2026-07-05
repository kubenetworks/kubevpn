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
