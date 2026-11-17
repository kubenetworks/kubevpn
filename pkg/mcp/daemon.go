package mcp

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

// connectStream is the bidirectional Connect stream client.
type connectStream = rpc.Daemon_ConnectClient

// proxyStreamClient is the bidirectional Proxy stream client.
type proxyStreamClient = rpc.Daemon_ProxyClient

// leaveStreamClient is the bidirectional Leave stream client.
type leaveStreamClient = rpc.Daemon_LeaveClient

// disconnectStreamClient is the bidirectional Disconnect stream client.
type disconnectStreamClient = rpc.Daemon_DisconnectClient

// daemonAPI is the slice of the daemon gRPC surface the MCP tools expose. It
// exists so handlers are testable against a fake; rpc.DaemonClient satisfies it.
type daemonAPI interface {
	Version(ctx context.Context, in *rpc.VersionRequest, opts ...grpc.CallOption) (*rpc.VersionResponse, error)
	Status(ctx context.Context, in *rpc.StatusRequest, opts ...grpc.CallOption) (*rpc.StatusResponse, error)
	ConnectionList(ctx context.Context, in *rpc.ConnectionListRequest, opts ...grpc.CallOption) (*rpc.ConnectionListResponse, error)
	Connect(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_ConnectClient, error)
	Proxy(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_ProxyClient, error)
	Leave(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_LeaveClient, error)
	Disconnect(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_DisconnectClient, error)
	Logs(ctx context.Context, opts ...grpc.CallOption) (rpc.Daemon_LogsClient, error)
}

var _ daemonAPI = rpc.DaemonClient(nil)

// progressStream is the read side of a streaming RPC call.
type progressStream interface {
	RecvMsg(msg any) error
}

// drainProgress consumes a streaming RPC until terminal state, returning the
// last message, every message, and the terminal error — io.EOF on success.
// Mirrors cmds/progress.go: the daemon closes the stream when the operation
// completes; sessions live daemon-side.
func drainProgress[T any](s progressStream) (last string, msgs []string, err error) {
	for {
		t := new(T)
		if err = s.RecvMsg(t); err != nil {
			return last, msgs, err // io.EOF on success; real errors propagate
		}
		if p, ok := any(t).(interface{ GetMessage() string }); ok {
			msg := p.GetMessage()
			msgs = append(msgs, msg)
			last = msg
		}
	}
}

// errElevationRequired indicates that the operation requires the root daemon
// to be running, and the user needs to be prompted to run it.
var errElevationRequired = errors.New("elevation required")

// elevationHint is the handoff instruction for the interactive sudo prompt,
// per docs/agents-guide.md.
const elevationHint = "the root daemon requires an interactive sudo prompt: " +
	"surface the command `sudo kubevpn daemon` to the human, wait for them to run it, then retry once"

// wrapHint attaches an actionable hint to a daemon failure. Never wraps the
// sentinel cancellation.
func wrapHint(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (hint: %s)", err, elevationHint)
}

// tail returns the last n strings, original order.
func tail(ss []string, n int) []string {
	if len(ss) > n {
		return ss[len(ss)-n:]
	}
	return ss
}
