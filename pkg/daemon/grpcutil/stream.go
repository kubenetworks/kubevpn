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

// ListenCancel waits for a Cancel message on the server stream and invokes the cancel function when received.
func ListenCancel(resp grpc.ServerStream, cancelFunc context.CancelFunc) {
	var s rpc.Cancel
	if resp.RecvMsg(&s) == nil {
		cancelFunc()
	}
}
