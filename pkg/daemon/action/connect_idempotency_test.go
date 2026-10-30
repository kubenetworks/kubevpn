package action

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
)

// fakeConnectServer is a minimal rpc.Daemon_ConnectServer that yields one request
// then EOF, and captures the responses streamed back. The embedded grpc.ServerStream
// stays nil — its methods are not exercised on the idempotency short-circuit path.
type fakeConnectServer struct {
	grpc.ServerStream
	ctx      context.Context
	req      *rpc.ConnectRequest
	received bool
	sent     []*rpc.ConnectResponse
}

func (f *fakeConnectServer) Send(r *rpc.ConnectResponse) error {
	f.sent = append(f.sent, r)
	return nil
}

func (f *fakeConnectServer) Recv() (*rpc.ConnectRequest, error) {
	if f.received {
		return nil, io.EOF
	}
	f.received = true
	return f.req, nil
}

func (f *fakeConnectServer) Context() context.Context { return f.ctx }
func (f *fakeConnectServer) RecvMsg(any) error        { return io.EOF }
func (f *fakeConnectServer) SendMsg(v any) error      { return f.Send(v.(*rpc.ConnectResponse)) }

type terminalTestSession struct {
	*handler.DataSession
	ctx     context.Context
	cleaned atomic.Int32
}

func (s *terminalTestSession) Context() context.Context { return s.ctx }
func (s *terminalTestSession) Cleanup(context.Context)  { s.cleaned.Add(1) }

func TestConnect_TerminalSessionDoesNotReportSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	old := &terminalTestSession{DataSession: &handler.DataSession{ConnectionID: "terminal"}, ctx: ctx}
	other := &handler.DataSession{ConnectionID: "other"}
	logFile := &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "daemon.log")}
	t.Cleanup(func() { _ = logFile.Close() })
	svr := &Server{IsSudo: true, LogFile: logFile, connections: []handler.Connection{old, other}}
	cancel()
	// Invalid credentials make rebuilding fail before any real network operation.
	// A terminal entry must not turn this failure into an idempotent success.
	err := svr.Connect(&fakeConnectServer{ctx: context.Background(), req: &rpc.ConnectRequest{ConnectionID: "terminal", KubeconfigBytes: "invalid"}})
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("terminal reconnect falsely succeeded: %v", err)
	}
	if old.cleaned.Load() != 1 {
		t.Fatal("terminal session cleanup was not joined")
	}
	if len(svr.connections) != 1 || svr.connections[0] != other {
		t.Fatal("terminal entry retained or unrelated session removed")
	}
}

// TestConnect_SudoIdempotentOnDuplicateConnectionID locks the B1 fix: when the root
// daemon already holds a data-plane session for a ConnectionID, a second Connect for
// the same ID (e.g. the user daemon restarted and LoadFromConfig replayed Connect while
// this root daemon survived) must short-circuit — NOT build a duplicate DataSession /
// second TUN / port-forward / route / DNS setup.
func TestConnect_SudoIdempotentOnDuplicateConnectionID(t *testing.T) {
	logFile := &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "daemon.log")}
	defer func() { _ = logFile.Close() }()

	const cid = "abc123def456"
	existing := &handler.DataSession{ConnectionID: cid}
	svr := &Server{
		IsSudo:      true,
		LogFile:     logFile,
		connections: []handler.Connection{existing},
	}
	resp := &fakeConnectServer{
		ctx: context.Background(),
		req: &rpc.ConnectRequest{ConnectionID: cid, Namespace: "default"},
	}

	if err := svr.Connect(resp); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}

	// No duplicate data plane: the connection slice is unchanged and still the same instance.
	if got := len(svr.connections); got != 1 {
		t.Fatalf("expected 1 connection (no duplicate created), got %d", got)
	}
	if svr.connections[0] != existing {
		t.Fatal("existing data-plane session must be preserved, not replaced")
	}

	// The short-circuit still reports success carrying the ConnectionID.
	if len(resp.sent) == 0 {
		t.Fatal("expected an idempotent success response, got none")
	}
	last := resp.sent[len(resp.sent)-1]
	if last.GetConnectionID() != cid {
		t.Fatalf("expected ConnectResponse with ConnectionID %q, got %q", cid, last.GetConnectionID())
	}
}
