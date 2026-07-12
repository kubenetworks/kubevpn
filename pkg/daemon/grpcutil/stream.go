package grpcutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util/progress"
)

// Printable is an interface for gRPC response messages that can provide a human-readable string.
type Printable interface {
	GetMessage() string
}

// PrintGRPCStream reads messages from a gRPC client stream and writes their content to the provided writers (or stdout).
func PrintGRPCStream[T any](ctx context.Context, clientStream grpc.ClientStream, writers ...io.Writer) error {
	var out io.Writer = os.Stdout
	for _, writer := range writers {
		out = writer
		break
	}

	if ctx != nil {
		// Watch for cancellation, but stop watching once this function returns
		// so the goroutine never outlives the stream (avoids a leak when ctx is
		// a never-cancelled context such as context.Background()).
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

	for {
		t := new(T)
		err := clientStream.RecvMsg(t)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if out == nil {
			continue
		}
		if p, ok := any(t).(Printable); ok {
			// Strip any CLI progress sentinel so non-spinner consumers (daemon log
			// file, `kubevpn run`, the `logs` command) never emit the raw \x1f
			// marker; the animated rendering lives in the CLI's printProgressStream.
			_, msg := plog.DecodeStep(p.GetMessage())
			_, _ = fmt.Fprint(out, msg)
		} else {
			buf, _ := json.Marshal(t)
			_, _ = fmt.Fprint(out, string(buf))
		}
	}
}

// RenderGRPCStream reads messages from a gRPC client stream and renders them as
// animated CLI progress via progress.Renderer: steps animate as a spinner and
// finalize with a check mark, while ordinary log lines scroll above. Unlike
// PrintGRPCStream — which strips the step sentinel for plain consumers (log file,
// `logs` command) — it forwards the raw message so the renderer can decode the
// step kind. Use it for interactive CLI commands (e.g. `kubevpn run`) that want
// the same progress UX as connect. The renderer degrades to plain, ✓-annotated
// lines off a TTY (pipes, CI).
func RenderGRPCStream[T any](ctx context.Context, clientStream grpc.ClientStream, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}

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

	r := progress.New(out)
	defer r.Stop()

	for {
		t := new(T)
		err := clientStream.RecvMsg(t)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if p, ok := any(t).(Printable); ok {
			if msg := p.GetMessage(); msg != "" {
				r.Write(msg)
			}
		}
	}
}

// CopyGRPCStream copies messages from a gRPC client stream to a server stream until EOF.
func CopyGRPCStream[T any](r grpc.ClientStream, w grpc.ServerStream) error {
	for {
		t := new(T)
		err := r.RecvMsg(t)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		err = w.SendMsg(t)
		if err != nil {
			return err
		}
	}
}

// CopyGRPCConnStream copies ConnectResponse messages from a bidirectional stream to a server stream, returning the connection ID.
func CopyGRPCConnStream(r grpc.BidiStreamingClient[rpc.ConnectRequest, rpc.ConnectResponse], w grpc.ServerStream) (string, error) {
	var connectionID string
	for {
		resp, err := r.Recv()
		if errors.Is(err, io.EOF) {
			return connectionID, nil
		}
		if err != nil {
			return connectionID, err
		}
		if resp.ConnectionID != "" {
			connectionID = resp.ConnectionID
		}
		err = w.SendMsg(resp)
		if err != nil {
			return connectionID, err
		}
	}
}

// CopyAndConvertGRPCStream copies messages from a client stream to a server stream, applying a conversion function to each message.
func CopyAndConvertGRPCStream[I any, O any](r grpc.ClientStream, w grpc.ServerStream, convert func(*I) *O) error {
	for {
		i := new(I)
		err := r.RecvMsg(i)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		o := convert(i)
		err = w.SendMsg(o)
		if err != nil {
			return err
		}
	}
}

// ListenCancel waits for an explicit Cancel message on the server stream and invokes cancelFunc
// only then.
//
// INVARIANT: cancelFunc runs iff RecvMsg returns nil (a real Cancel message arrived). Any error
// — including io.EOF when the stream closes normally after its handler returns — must NOT cancel.
// This is what lets a data-plane session (e.g. root-daemon Connect) keep running after its RPC
// returns: the leftover ListenCancel goroutine unblocks with io.EOF and exits WITHOUT tearing
// the session down. Do not "simplify" this to cancel on any RecvMsg return. Regression-guarded by
// TestListenCancel_CancelsOnlyOnExplicitMessage.
func ListenCancel(resp grpc.ServerStream, cancelFunc context.CancelFunc) {
	var s rpc.Cancel
	if resp.RecvMsg(&s) == nil {
		cancelFunc()
	}
}
