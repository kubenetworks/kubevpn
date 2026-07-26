package grpcutil

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// fakeClientStream replays a fixed list of messages as ConnectResponse.Message,
// then returns io.EOF — enough to drive PrintGRPCStream without a real gRPC conn.
type fakeClientStream struct {
	msgs []string
	i    int
}

func (f *fakeClientStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeClientStream) Trailer() metadata.MD         { return nil }
func (f *fakeClientStream) CloseSend() error             { return nil }
func (f *fakeClientStream) Context() context.Context     { return context.Background() }
func (f *fakeClientStream) SendMsg(any) error            { return nil }
func (f *fakeClientStream) RecvMsg(m any) error {
	if f.i >= len(f.msgs) {
		return io.EOF
	}
	m.(*rpc.ConnectResponse).Message = f.msgs[f.i]
	f.i++
	return nil
}

// TestPrintGRPCStream_StripsStepSentinel verifies that non-spinner consumers
// (daemon log file, `kubevpn run`, the `logs` command) never see the raw \x1f
// step marker: PrintGRPCStream decodes it away while keeping the message text.
func TestPrintGRPCStream_StripsStepSentinel(t *testing.T) {
	stream := &fakeClientStream{msgs: []string{
		plog.EncodeStep(plog.StepBegin, "Forwarding ports\n"),
		plog.EncodeStep(plog.StepEnd, "Forwarded ports (TCP/UDP/xDS)\n"),
		"an ordinary line\n",
	}}
	var buf strings.Builder
	if err := PrintGRPCStream[rpc.ConnectResponse](nil, stream, &buf); err != nil {
		t.Fatalf("PrintGRPCStream: %v", err)
	}
	out := buf.String()
	if strings.ContainsRune(out, '\x1f') {
		t.Fatalf("output must not contain the step sentinel: %q", out)
	}
	for _, want := range []string{"Forwarding ports", "Forwarded ports (TCP/UDP/xDS)", "an ordinary line"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
}
