package progress

import (
	"bytes"
	"os"
	"strings"
	"testing"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// spinnerFrameGlyphs are the braille animation characters yacspin cycles through
// (CharSets[11]). None of them may appear in non-TTY output: off a TTY the
// Renderer must not build the animated spinner, so a step settles to a single
// " ✓ text" line with no intermediate frames.
const spinnerFrameGlyphs = "⣾⣽⣻⢿⡿⣟⣯⣷⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

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
	// The cursor-erase sequence used to clean up the spinner line on a TTY must
	// never leak into a non-terminal writer (pipes, CI, log capture).
	if strings.Contains(out, "\x1b[K") {
		t.Errorf("non-TTY output must not contain the erase escape sequence, got:\n%q", out)
	}
	// Regression: the animated spinner must not be built off a TTY, so no braille
	// animation frame ("⣾ Forwarding ports") may leak, and the step must settle to
	// exactly one "✓" line — not the noisy multi-frame output yacspin's own non-TTY
	// degrade produced (see docs/30 §4).
	if i := strings.IndexAny(out, spinnerFrameGlyphs); i >= 0 {
		t.Errorf("non-TTY output must not contain spinner frame glyphs, got:\n%q", out)
	}
	if n := strings.Count(out, "✓"); n != 1 {
		t.Errorf("expected exactly one ✓ line for one step, got %d in:\n%s", n, out)
	}
	// The in-progress start text has no standalone line off a TTY; only the done
	// text is rendered (on the single ✓ line).
	if strings.Contains(out, "\n Forwarding ports") || strings.HasPrefix(out, " Forwarding ports") {
		t.Errorf("non-TTY output must not print the in-progress start line, got:\n%s", out)
	}
}

// TestRenderer_NonTTY_PlainOnly ensures a stream with no step markers (e.g. an
// ordinary log line) is passed through verbatim.
func TestRenderer_NonTTY_PlainOnly(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Write("an ordinary log line\n")
	r.Stop()
	if got := buf.String(); got != "an ordinary log line\n" {
		t.Fatalf("plain line should pass through verbatim, got %q", got)
	}
}

// TestRenderer_NonTTY_Heading verifies a heading renders as plain text (no ANSI,
// no spinner, no check mark) on a non-terminal writer.
func TestRenderer_NonTTY_Heading(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.Write(plog.EncodeStep(plog.StepHeading, "Connecting to the cluster ...") + "\n")
	r.Stop()
	if got := buf.String(); got != "Connecting to the cluster ...\n" {
		t.Fatalf("heading should be plain on non-TTY, got %q", got)
	}
}

// TestSuccess_NonTTY verifies the success terminus prints plain text (no ANSI)
// on a non-terminal writer, so log scrapers still match config.Slogan verbatim.
func TestSuccess_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	Success(&buf, "Connected. You can now access the Kubernetes cluster.")
	if got := buf.String(); got != "Connected. You can now access the Kubernetes cluster.\n" {
		t.Fatalf("success should be plain on non-TTY, got %q", got)
	}
}

// TestSmartTTY covers the single terminal-capability predicate that gates both the
// animated spinner and all coloring.
func TestSmartTTY(t *testing.T) {
	// A *bytes.Buffer is not an *os.File — never a smart TTY.
	if smartTTY(&bytes.Buffer{}) {
		t.Error("bytes.Buffer must not be a smart TTY")
	}

	// An *os.File that is a pipe (not a terminal) is not a smart TTY.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()
	if smartTTY(pw) {
		t.Error("a pipe *os.File must not be a smart TTY")
	}

	// A real terminal (pts slave) is a smart TTY when TERM != "dumb", and not when
	// it is. openPTY is only implemented on Linux (no extra dependency); elsewhere
	// this portion is skipped — the smart path is yacspin's, exercised by CI Linux.
	master, tty, ok := openPTY(t)
	if !ok {
		t.Skip("no PTY on this platform; buffer/pipe cases already covered above")
	}
	defer master.Close()
	defer tty.Close()

	t.Setenv("TERM", "xterm-256color")
	if !smartTTY(tty) {
		t.Error("a real terminal with TERM=xterm-256color must be a smart TTY")
	}
	t.Setenv("TERM", "dumb")
	if smartTTY(tty) {
		t.Error("TERM=dumb must not be treated as a smart TTY")
	}
}
