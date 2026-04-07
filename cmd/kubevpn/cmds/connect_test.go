package cmds

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
)

type fakeConnectClientStream struct {
	ctx        context.Context
	responses  []*rpc.ConnectResponse
	recvIndex  int
	cancelSent int
	waitOnDone bool
}

func (f *fakeConnectClientStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeConnectClientStream) Trailer() metadata.MD         { return nil }
func (f *fakeConnectClientStream) CloseSend() error             { return nil }
func (f *fakeConnectClientStream) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}
func (f *fakeConnectClientStream) SendMsg(m interface{}) error {
	if _, ok := m.(*rpc.Cancel); ok {
		f.cancelSent++
	}
	return nil
}
func (f *fakeConnectClientStream) RecvMsg(m interface{}) error {
	if f.recvIndex >= len(f.responses) {
		if f.waitOnDone && f.ctx != nil {
			<-f.ctx.Done()
			time.Sleep(10 * time.Millisecond)
		}
		return io.EOF
	}
	resp, ok := m.(*rpc.ConnectResponse)
	if !ok {
		return io.ErrUnexpectedEOF
	}
	*resp = *f.responses[f.recvIndex]
	f.recvIndex++
	return nil
}

func TestPrintConnectGRPCStreamReturnsConnectionID(t *testing.T) {
	stream := &fakeConnectClientStream{
		responses: []*rpc.ConnectResponse{
			{Message: "Starting connect\n"},
			{ConnectionID: "conn-123", Message: "Connected tunnel\n"},
		},
	}
	var out bytes.Buffer

	connectionID, err := printConnectGRPCStream(context.Background(), stream, &out)
	if err != nil {
		t.Fatal(err)
	}
	if connectionID != "conn-123" {
		t.Fatalf("connectionID = %q, want %q", connectionID, "conn-123")
	}
	if got := out.String(); got != "Starting connect\nConnected tunnel\n" {
		t.Fatalf("unexpected output %q", got)
	}
}

func TestPrintConnectGRPCStreamSendsCancelOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream := &fakeConnectClientStream{ctx: ctx, waitOnDone: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = printConnectGRPCStream(ctx, stream, io.Discard)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("printConnectGRPCStream did not finish after cancel")
	}

	if stream.cancelSent == 0 {
		t.Fatal("expected cancel message to be sent to the grpc stream")
	}
}
