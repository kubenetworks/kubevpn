package action

import (
	"context"
	"io"
	"sort"
	"testing"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// fakeDisconnectStream records the ConnectionID sent to the sudo daemon and closes the
// stream immediately (RecvMsg → io.EOF) so PrintGRPCStream returns without blocking.
type fakeDisconnectStream struct {
	rpc.Daemon_DisconnectClient
	sent *[]string
}

func (f *fakeDisconnectStream) Send(req *rpc.DisconnectRequest) error {
	*f.sent = append(*f.sent, req.GetConnectionID())
	return nil
}

func (f *fakeDisconnectStream) RecvMsg(any) error { return io.EOF }

// fakeSudoClient is a minimal rpc.DaemonClient exposing only the two methods
// ReconcileSudoConnections uses: Status (returns a fixed snapshot) and Disconnect
// (records the reaped IDs). All other methods are nil (never called).
type fakeSudoClient struct {
	rpc.DaemonClient
	statusResp   *rpc.StatusResponse
	disconnected *[]string
}

func (f *fakeSudoClient) Status(context.Context, *rpc.StatusRequest, ...grpc.CallOption) (*rpc.StatusResponse, error) {
	return f.statusResp, nil
}

func (f *fakeSudoClient) Disconnect(context.Context, ...grpc.CallOption) (rpc.Daemon_DisconnectClient, error) {
	return &fakeDisconnectStream{sent: f.disconnected}, nil
}

// TestReconcileSudoConnections_HealthFiltered verifies the orphan reaper reaps ONLY sudo
// connections that are both absent from the user daemon's set AND not reporting "connected":
//   - orphan + unhealthy       → reaped
//   - orphan + disconnected    → reaped
//   - orphan + connected       → spared (racing an in-flight connect must not be killed)
//   - tracked + unhealthy      → spared (the user daemon owns it)
func TestReconcileSudoConnections_HealthFiltered(t *testing.T) {
	var disconnected []string
	statusResp := &rpc.StatusResponse{List: []*rpc.Status{
		{ConnectionID: "orphan-unhealthy", Status: StatusUnhealthy},
		{ConnectionID: "orphan-disconnected", Status: StatusFailed},
		{ConnectionID: "orphan-connected", Status: StatusOk},
		{ConnectionID: "tracked-unhealthy", Status: StatusUnhealthy},
	}}
	fake := &fakeSudoClient{statusResp: statusResp, disconnected: &disconnected}

	svr := &Server{
		// User daemon owns "tracked-unhealthy" only.
		connections: []handler.Connection{
			&handler.ConnectOptions{ConnectionID: "tracked-unhealthy"},
		},
		GetClient: func(isSudo bool) (rpc.DaemonClient, error) { return fake, nil },
	}

	svr.ReconcileSudoConnections(context.Background())

	sort.Strings(disconnected)
	want := []string{"orphan-disconnected", "orphan-unhealthy"}
	if len(disconnected) != len(want) {
		t.Fatalf("expected reaped %v, got %v", want, disconnected)
	}
	for i := range want {
		if disconnected[i] != want[i] {
			t.Fatalf("expected reaped %v, got %v", want, disconnected)
		}
	}
}

// TestReconcileSudoConnections_SudoOnlyNoop verifies the reaper is a no-op on the sudo
// daemon itself (it must only run from the user daemon).
func TestReconcileSudoConnections_SudoOnlyNoop(t *testing.T) {
	called := false
	svr := &Server{
		IsSudo: true,
		GetClient: func(isSudo bool) (rpc.DaemonClient, error) {
			called = true
			return nil, nil
		},
	}
	svr.ReconcileSudoConnections(context.Background())
	if called {
		t.Fatal("ReconcileSudoConnections must not touch the sudo daemon when IsSudo")
	}
}
