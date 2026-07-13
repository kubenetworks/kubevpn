package action

import (
	"context"
	"io"
	"path/filepath"
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
