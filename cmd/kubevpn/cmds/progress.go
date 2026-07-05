package cmds

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/grpcutil"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/progress"
)

// connectionIDer lets printProgressStream capture a connection ID from a
// response type without knowing its concrete type. Only ConnectResponse (and
// types that embed it) carry one; everything else simply never matches.
type connectionIDer interface {
	GetConnectionID() string
}

// printProgressStream consumes a daemon gRPC stream and renders it as kind-style
// progress: steps (carrying the \x1f sentinel) animate as a spinner and finalize
// with a check mark, while ordinary log lines scroll above (see
// pkg/util/progress and docs/30-connect-progress.md). It is the single CLI-side
// renderer shared by every step-bearing command.
//
// The returned string is the last ConnectionID seen on the stream (empty when
// the response type does not carry one). When out is nil, messages are drained
// without rendering.
func printProgressStream[T any](ctx context.Context, clientStream grpc.ClientStream, out io.Writer) (string, error) {
	if ctx != nil {
		// Forward CLI cancellation (Ctrl-C) to the daemon, but stop watching once
		// this function returns so the goroutine never outlives the stream.
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = clientStream.SendMsg(&rpc.Cancel{})
			case <-done:
			}
		}()
	}

	var r *progress.Renderer
	if out != nil {
		r = progress.New(out)
		defer r.Stop()
	}

	var connectionID string
	for {
		t := new(T)
		err := clientStream.RecvMsg(t)
		if errors.Is(err, io.EOF) {
			return connectionID, nil
		}
		if err != nil {
			return connectionID, err
		}
		if c, ok := any(t).(connectionIDer); ok {
			if id := c.GetConnectionID(); id != "" {
				connectionID = id
			}
		}
		if r != nil {
			if p, ok := any(t).(grpcutil.Printable); ok && p.GetMessage() != "" {
				r.Write(p.GetMessage())
			}
		}
	}
}
