package cmds

import (
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// fakeProgressStream replays a fixed list of ConnectResponses, then io.EOF.
type fakeProgressStream struct {
	resps []*rpc.ConnectResponse
	i     int
}

func (f *fakeProgressStream) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeProgressStream) Trailer() metadata.MD         { return nil }
func (f *fakeProgressStream) CloseSend() error             { return nil }
func (f *fakeProgressStream) Context() context.Context     { return context.Background() }
func (f *fakeProgressStream) SendMsg(any) error            { return nil }
func (f *fakeProgressStream) RecvMsg(m any) error {
	if f.i >= len(f.resps) {
		return io.EOF
	}
	*(m.(*rpc.ConnectResponse)) = *f.resps[f.i]
	f.i++
	return nil
}

// TestPrintProgressStream_RendersStepsAndCapturesConnID drives the shared CLI
// renderer end to end against a fake stream: a *strings.Builder is not a TTY, so
// the renderer degrades to line-by-line output, strips the step sentinel, prints
// the finished step with a check mark, and the ConnectionID is captured.
func TestPrintProgressStream_RendersStepsAndCapturesConnID(t *testing.T) {
	stream := &fakeProgressStream{resps: []*rpc.ConnectResponse{
		{Message: plog.EncodeStep(plog.StepBegin, "Forwarding ports\n")},
		{Message: plog.EncodeStep(plog.StepEnd, "Forwarded ports (TCP/UDP/xDS)\n"), ConnectionID: "abc123def456"},
	}}
	var buf strings.Builder
	id, err := printProgressStream[rpc.ConnectResponse](context.Background(), stream, &buf)
	if err != nil {
		t.Fatalf("printProgressStream: %v", err)
	}
	if id != "abc123def456" {
		t.Fatalf("connID = %q, want abc123def456", id)
	}
	out := buf.String()
	if strings.ContainsRune(out, '\x1f') {
		t.Errorf("rendered output must not contain the step sentinel: %q", out)
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "Forwarded ports (TCP/UDP/xDS)") {
		t.Errorf("expected check mark + done text, got: %q", out)
	}
}
