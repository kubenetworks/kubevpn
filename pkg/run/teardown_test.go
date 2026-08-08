package run

import (
	"context"
	"io"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	pkgssh "github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

// fakeClientStream provides the grpc.ClientStream surface RenderGRPCStream needs
// (RecvMsg returns EOF immediately so the renderer drains and returns). The other
// methods are never exercised by teardownRunProxy.
type fakeClientStream struct{}

func (fakeClientStream) Header() (metadata.MD, error) { return nil, nil }
func (fakeClientStream) Trailer() metadata.MD         { return nil }
func (fakeClientStream) CloseSend() error             { return nil }
func (fakeClientStream) Context() context.Context     { return context.Background() }
func (fakeClientStream) SendMsg(any) error            { return nil }
func (fakeClientStream) RecvMsg(any) error            { return io.EOF }

type fakeLeaveStream struct {
	fakeClientStream
	rec *teardownRecorder
}

func (f *fakeLeaveStream) Send(r *rpc.LeaveRequest) error {
	f.rec.order = append(f.rec.order, "leave")
	f.rec.leaveReq = r
	return nil
}
func (f *fakeLeaveStream) Recv() (*rpc.LeaveResponse, error) { return nil, io.EOF }

type fakeDisconnectStream struct {
	fakeClientStream
	rec *teardownRecorder
}

func (f *fakeDisconnectStream) Send(r *rpc.DisconnectRequest) error {
	f.rec.order = append(f.rec.order, "disconnect")
	f.rec.disconnectReq = r
	return nil
}
func (f *fakeDisconnectStream) Recv() (*rpc.DisconnectResponse, error) { return nil, io.EOF }

// teardownRecorder captures the calls teardownRunProxy makes against the daemon.
type teardownRecorder struct {
	rpc.DaemonClient // embedded (nil): only Leave/Disconnect are implemented
	order            []string
	leaveReq         *rpc.LeaveRequest
	disconnectReq    *rpc.DisconnectRequest
	leaveErr         error
}

func (r *teardownRecorder) Leave(context.Context, ...grpc.CallOption) (rpc.Daemon_LeaveClient, error) {
	if r.leaveErr != nil {
		return nil, r.leaveErr
	}
	return &fakeLeaveStream{rec: r}, nil
}

func (r *teardownRecorder) Disconnect(context.Context, ...grpc.CallOption) (rpc.Daemon_DisconnectClient, error) {
	return &fakeDisconnectStream{rec: r}, nil
}

// TestTeardownRunProxy_LeaveBeforeDisconnect verifies the run teardown contract:
// when proxying, it leaves the proxy resources BEFORE disconnecting (so the
// injected sidecars are reliably unpatched), and forwards the right namespace +
// workload. This mirrors the proven `kubevpn proxy --foreground` order.
func TestTeardownRunProxy_LeaveBeforeDisconnect(t *testing.T) {
	rec := &teardownRecorder{}
	err := teardownRunProxy(rec, false, "default", "deploy/authors", []byte("kubeconfig"), &pkgssh.SshConfig{})
	if err != nil {
		t.Fatalf("teardownRunProxy: %v", err)
	}

	want := []string{"leave", "disconnect"}
	if len(rec.order) != 2 || rec.order[0] != want[0] || rec.order[1] != want[1] {
		t.Fatalf("teardown order = %v, want %v (leave must precede disconnect)", rec.order, want)
	}
	if rec.leaveReq.GetNamespace() != "default" {
		t.Errorf("LeaveRequest namespace = %q, want %q", rec.leaveReq.GetNamespace(), "default")
	}
	if got := rec.leaveReq.GetWorkloads(); len(got) != 1 || got[0] != "deploy/authors" {
		t.Errorf("LeaveRequest workloads = %v, want [deploy/authors]", got)
	}
	if rec.disconnectReq.GetNamespace() != "default" {
		t.Errorf("DisconnectRequest namespace = %q, want %q", rec.disconnectReq.GetNamespace(), "default")
	}
}

// TestTeardownRunProxy_NoProxySkipsLeave verifies that --no-proxy runs leave a
// no-op: there is no injected sidecar to unpatch, so teardown only disconnects.
func TestTeardownRunProxy_NoProxySkipsLeave(t *testing.T) {
	rec := &teardownRecorder{}
	err := teardownRunProxy(rec, true, "default", "deploy/authors", []byte("kubeconfig"), &pkgssh.SshConfig{})
	if err != nil {
		t.Fatalf("teardownRunProxy: %v", err)
	}
	if len(rec.order) != 1 || rec.order[0] != "disconnect" {
		t.Fatalf("teardown order = %v, want [disconnect] only (no-proxy skips leave)", rec.order)
	}
}

// TestTeardownRunProxy_LeaveErrorStillDisconnects verifies leave is best-effort:
// if the Leave RPC fails, teardown still disconnects so the VPN tears down.
func TestTeardownRunProxy_LeaveErrorStillDisconnects(t *testing.T) {
	rec := &teardownRecorder{leaveErr: io.ErrUnexpectedEOF}
	err := teardownRunProxy(rec, false, "default", "deploy/authors", []byte("kubeconfig"), &pkgssh.SshConfig{})
	if err != nil {
		t.Fatalf("teardownRunProxy should swallow leave errors, got: %v", err)
	}
	if len(rec.order) != 1 || rec.order[0] != "disconnect" {
		t.Fatalf("teardown order = %v, want [disconnect] after leave failure", rec.order)
	}
}

// TestConfigList_Run_CleanExitOnCancel verifies that a cancelled context (user
// Ctrl-C) makes ConfigList.Run return nil instead of a spurious
// "signal: killed: docker run failed" — the foreground container kill is the
// intended shutdown, not an error.
func TestConfigList_Run_CleanExitOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: RunContainer's docker exec fails, but ctx.Err() != nil

	l := ConfigList{{name: "dev", image: "busybox"}}
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run on cancelled ctx should return nil (clean shutdown), got: %v", err)
	}
}
