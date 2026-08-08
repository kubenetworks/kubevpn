package grpcutil

import (
	"bytes"
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

// TestRenderGRPCStream_KeepsStepSentinelForSpinner verifies the spinner-rendering
// counterpart used by `kubevpn run`: it forwards the step sentinel to the
// progress.Renderer, so a finished step lands as a "✓ <done text>" line (and the
// raw \x1f marker never leaks). This is what gives `run` the same check-mark UX as
// `connect`, instead of the plain double-line PrintGRPCStream produced.
func TestRenderGRPCStream_KeepsStepSentinelForSpinner(t *testing.T) {
	stream := &fakeClientStream{msgs: []string{
		plog.EncodeStep(plog.StepBegin, "Forwarding ports\n"),
		"an ordinary line\n",
		plog.EncodeStep(plog.StepEnd, "Forwarded ports (TCP/UDP/xDS)\n"),
	}}
	// A *bytes.Buffer is not a TTY, so the renderer degrades to plain, ✓-annotated
	// lines (no spinner animation, no cursor-erase escapes).
	var buf bytes.Buffer
	if err := RenderGRPCStream[rpc.ConnectResponse](nil, stream, &buf); err != nil {
		t.Fatalf("RenderGRPCStream: %v", err)
	}
	out := buf.String()
	if strings.ContainsRune(out, '\x1f') {
		t.Fatalf("output must not contain the step sentinel: %q", out)
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "Forwarded ports (TCP/UDP/xDS)") {
		t.Errorf("finished step should render a check mark + its done text, got: %q", out)
	}
	if !strings.Contains(out, "an ordinary line") {
		t.Errorf("ordinary log line should pass through, got: %q", out)
	}
	if strings.Contains(out, "\x1b[K") {
		t.Errorf("non-TTY output must not contain the cursor-erase escape, got: %q", out)
	}
}
