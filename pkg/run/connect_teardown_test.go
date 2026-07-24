package run

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// blockingDaemon is a real gRPC DaemonServer whose Leave/Disconnect handlers never respond
// until release is closed — simulating a daemon wedged on an unreachable cluster. It lets us
// verify, over a real gRPC client↔server, that teardownRunProxy is time-bounded and does not
// hang the client (the failure mode that turned a dead-cluster hiccup into a 2h test hang).
type blockingDaemon struct {
	rpc.UnimplementedDaemonServer
	release chan struct{}
}

func (b *blockingDaemon) Leave(rpc.Daemon_LeaveServer) error {
	<-b.release
	return nil
}

func (b *blockingDaemon) Disconnect(rpc.Daemon_DisconnectServer) error {
	<-b.release
	return nil
}

// newBlockingDaemonClient starts an in-process gRPC server backed by blockingDaemon on a
// loopback socket and returns a connected DaemonClient. All resources are cleaned up via t.
func newBlockingDaemonClient(t *testing.T) rpc.DaemonClient {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // unblock handler goroutines so they can exit

	srv := grpc.NewServer()
	rpc.RegisterDaemonServer(srv, &blockingDaemon{release: release})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.DialContext(context.Background(),
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return rpc.NewDaemonClient(conn)
}

// TestTeardownRunProxy_BoundedAgainstWedgedDaemon verifies that when the daemon's leave and
// disconnect never return (dead cluster), the client-side teardown still returns within the
// leave+disconnect timeout budget instead of blocking on context.Background() forever.
func TestTeardownRunProxy_BoundedAgainstWedgedDaemon(t *testing.T) {
	origLeave, origDisconnect := leaveTeardownTimeout, disconnectTeardownTimeout
	leaveTeardownTimeout = 300 * time.Millisecond
	disconnectTeardownTimeout = 300 * time.Millisecond
	defer func() {
		leaveTeardownTimeout, disconnectTeardownTimeout = origLeave, origDisconnect
	}()

	cli := newBlockingDaemonClient(t)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- teardownRunProxy(cli, false, "default", "deploy/authors", []byte("dummy-kubeconfig"), nil)
	}()

	budget := leaveTeardownTimeout + disconnectTeardownTimeout
	select {
	case err := <-done:
		elapsed := time.Since(start)
		// The disconnect stream aborts on its deadline, so teardownRunProxy surfaces an
		// error — the point is that it RETURNS (bounded), not that it succeeds.
		if err == nil {
			t.Fatal("expected a deadline error from the wedged disconnect, got nil")
		}
		if elapsed > budget+5*time.Second {
			t.Fatalf("teardownRunProxy took %s, exceeds budget %s + slack", elapsed, budget)
		}
	case <-time.After(budget + 10*time.Second):
		t.Fatal("teardownRunProxy did not return — client-side teardown is not bounded")
	}
}

// TestLeaveRunProxy_BoundedAgainstWedgedDaemon isolates the leave path: a leave whose
// daemon never responds must still return within leaveTeardownTimeout.
func TestLeaveRunProxy_BoundedAgainstWedgedDaemon(t *testing.T) {
	orig := leaveTeardownTimeout
	leaveTeardownTimeout = 300 * time.Millisecond
	defer func() { leaveTeardownTimeout = orig }()

	cli := newBlockingDaemonClient(t)

	done := make(chan error, 1)
	go func() { done <- leaveRunProxy(cli, "default", "deploy/authors") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a deadline error from the wedged leave, got nil")
		}
	case <-time.After(leaveTeardownTimeout + 10*time.Second):
		t.Fatal("leaveRunProxy did not return — leave is not bounded")
	}
}
