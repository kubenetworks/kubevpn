package action

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// TestMonitorSudoHealth_ReportRecoverNoRestart drives the A1 liveness monitor through the full
// story a single connection sees when the root daemon degrades and recovers, and verifies the
// verdict the user daemon serves from its cached snapshot each step:
//
//	healthy probe            -> connected
//	transient probe failure  -> unhealthy (keep last-known conn, don't flap to disconnected)
//	root daemon gone         -> disconnected
//	root daemon back         -> connected
//
// The monitor only REPORTS: it never attempts to respawn the root daemon (there is no such hook
// on Server — recovery is the next CLI command's job).
func TestMonitorSudoHealth_ReportRecoverNoRestart(t *testing.T) {
	orig := sudoProcessAlive
	sudoProcessAlive = func() bool { return true } // bypass the real PID file; exercise the probe path
	defer func() { sudoProcessAlive = orig }()

	okResp := &rpc.StatusResponse{List: []*rpc.Status{
		{ConnectionID: "c1", IPv4: "198.19.0.5", Status: StatusOk},
	}}
	ctx := context.Background()
	svr := &Server{}
	conn := &handler.ConnectOptions{ConnectionID: "c1"}

	step := func(name, want string) {
		t.Helper()
		svr.refreshSudoHealth(ctx)
		got := resolveStatus(conn, svr.sudoHealthSnapshot(ctx), false)
		if got != want {
			t.Fatalf("%s: want %q, got %q", name, want, got)
		}
	}

	svr.GetClient = func(bool) (rpc.DaemonClient, error) { return &statusOKClient{resp: okResp}, nil }
	step("healthy", StatusOk)

	svr.GetClient = func(bool) (rpc.DaemonClient, error) {
		return &statusErrClient{err: status.Error(codes.DeadlineExceeded, "slow")}, nil
	}
	step("transient", StatusUnhealthy)

	svr.GetClient = func(bool) (rpc.DaemonClient, error) { return nil, fmt.Errorf("stat: %w", config.ErrDaemonNotRunning) }
	step("root down", StatusFailed)

	svr.GetClient = func(bool) (rpc.DaemonClient, error) { return &statusOKClient{resp: okResp}, nil }
	step("recovery", StatusOk)
}

// TestMonitorSudoHealth_PidDeadSkipsDialAndReportsDisconnected locks the crash-safe PID
// pre-check: when the root daemon process is gone, the monitor reports disconnected WITHOUT a
// (doomed) gRPC dial.
func TestMonitorSudoHealth_PidDeadSkipsDialAndReportsDisconnected(t *testing.T) {
	orig := sudoProcessAlive
	sudoProcessAlive = func() bool { return false }
	defer func() { sudoProcessAlive = orig }()

	ctx := context.Background()
	svr := &Server{
		GetClient: func(bool) (rpc.DaemonClient, error) {
			t.Fatal("must not dial the sudo daemon when its PID is not alive")
			return nil, nil
		},
	}
	conn := &handler.ConnectOptions{ConnectionID: "c1"}

	svr.refreshSudoHealth(ctx)
	if got := resolveStatus(conn, svr.sudoHealthSnapshot(ctx), false); got != StatusFailed {
		t.Fatalf("pid dead: want %q, got %q", StatusFailed, got)
	}
}
