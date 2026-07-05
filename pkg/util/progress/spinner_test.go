package progress

import (
	"bytes"
	"strings"
	"testing"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// TestRenderer_NonTTY verifies the non-TTY behavior end to end: a *bytes.Buffer
// is not a terminal, so yacspin degrades to non-animated, line-by-line output.
// We assert on substrings (yacspin owns the exact framing): the ordinary log line
// passes through, and the finished step is rendered with a check mark + its
// done text.
func TestRenderer_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)

	r.Write(plog.EncodeStep(plog.StepBegin, "Forwarding ports") + "\n")
	r.Write("an ordinary log line\n")
	r.Write(plog.EncodeStep(plog.StepEnd, "Forwarded ports (TCP/UDP/xDS)") + "\n")
	r.Stop()

	out := buf.String()
	if !strings.Contains(out, "an ordinary log line") {
		t.Errorf("ordinary log line should pass through, got:\n%s", out)
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "Forwarded ports (TCP/UDP/xDS)") {
		t.Errorf("finished step should render a check mark and its done text, got:\n%s", out)
	}
}

// TestRenderer_NonTTY_PlainOnly ensures a stream with no step markers (e.g. the
// header line) is passed through verbatim.
func TestRenderer_NonTTY_PlainOnly(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Write("Connecting to the cluster ...\n")
	r.Stop()
	if got := buf.String(); got != "Connecting to the cluster ...\n" {
		t.Fatalf("plain line should pass through verbatim, got %q", got)
	}
}
