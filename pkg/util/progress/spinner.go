// Package progress renders kind-style connect progress on the CLI: active steps
// animate as a spinner and finalize with a check mark, while ordinary log lines
// scroll above. The animation and Windows VT handling are delegated to
// github.com/theckman/yacspin. The spinner is built only on a smart TTY (see
// smartTTY); off a TTY / on a dumb terminal the Renderer uses a deterministic
// plain fallback (one " ✓ text" line per step) instead of yacspin's noisier
// non-TTY degrade.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/theckman/yacspin"
	"golang.org/x/term"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// ANSI styles for the non-spinner lines (heading, success terminus). They are
// emitted only on a smart TTY (see smartTTY / styleLine), the same condition
// under which yacspin colors the step check marks — so heading, ✓ steps, and the
// success line are colored together, or all plain together.
const (
	ansiBold      = "\x1b[1m"
	ansiBoldGreen = "\x1b[1;32m"
	ansiReset     = "\x1b[0m"
)

// smartTTY reports whether w is an interactive terminal that supports cursor
// control and color — i.e. one where the animated, colored spinner renders
// correctly. It mirrors yacspin's own smart-mode decision (interactive TTY and
// TERM != "dumb") so that our styling (bold heading, bold-green slogan) and
// yacspin's colored ✓ are gated identically: everything is colored on a smart
// TTY, everything is plain otherwise. Non-terminal writers (pipes, files, test
// buffers, CI) and dumb terminals get plain, un-styled output.
func smartTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
}

// styleLine writes msg + newline wrapped in the ANSI code on a smart TTY, plain
// otherwise.
func styleLine(w io.Writer, code, msg string) {
	if smartTTY(w) {
		fmt.Fprintf(w, "%s%s%s\n", code, msg, ansiReset)
	} else {
		fmt.Fprintln(w, msg)
	}
}

// Success prints the connection-success message as a bold-green terminus,
// echoing the green check marks of the steps above it. On non-TTY output it
// prints plainly (so log scrapers still match the raw text).
func Success(w io.Writer, msg string) {
	styleLine(w, ansiBoldGreen, msg)
}

// Renderer consumes the daemon message stream (one line per Write) and renders
// progress. Write is expected to be called serially from a single stream-receive
// loop; yacspin owns the animation goroutine and is safe for concurrent use.
type Renderer struct {
	out     io.Writer
	spinner *yacspin.Spinner // nil → plain fallback (spinner construction failed)
	running bool

	// Plain-path (spinner == nil) step tracking, used to render a heartbeat: the
	// first StepBegin of a step is suppressed (so a fast step stays a single ✓),
	// but a re-begin with new text (a long-running step's status update) prints one
	// " ○ text" line. open is true between a step's first begin and its end.
	open     bool
	lastText string
}

// New returns a Renderer writing to out. The animated yacspin spinner is built
// only when out is a smart TTY; off a TTY (pipes, files, CI) or on a dumb
// terminal the spinner is left nil and the Renderer uses its deterministic plain
// fallback (one " ✓ text" line per finished step, no animation frames, no control
// bytes). Relying on yacspin's own non-TTY degrade instead leaks braille
// animation frames as extra lines (" ⣾ Forwarding ports" before each "✓").
func New(out io.Writer) *Renderer {
	if !smartTTY(out) {
		return &Renderer{out: out}
	}
	s, err := yacspin.New(yacspin.Config{
		Frequency:         100 * time.Millisecond,
		Writer:            out,
		CharSet:           yacspin.CharSets[11],
		Prefix:            " ",
		Suffix:            " ",
		StopCharacter:     "✓",
		StopColors:        []string{"fgGreen"},
		StopFailCharacter: "✗",
		StopFailColors:    []string{"fgRed"},
	})
	if err != nil {
		return &Renderer{out: out}
	}
	return &Renderer{out: out, spinner: s}
}

// Write consumes one streamed message (one daemon log line, possibly carrying a
// step sentinel) and renders it.
func (r *Renderer) Write(message string) {
	kind, text := plog.DecodeStep(message)
	text = strings.TrimRight(text, "\n")

	switch kind {
	case plog.StepHeading:
		// A bold heading: no spinner, no check mark.
		r.printAboveSpinner(func() { styleLine(r.out, ansiBold, text) })
	case plog.StepBegin:
		if r.spinner == nil {
			// Plain-path heartbeat: print a " ○ text" line only for a re-begin of an
			// already-open step (a long-running step's changed status). The first begin
			// is suppressed so a fast step settles to a single ✓; the matching StepEnd
			// prints that line.
			if r.open && text != r.lastText {
				fmt.Fprintf(r.out, " ○ %s\n", text)
			}
			r.open = true
			r.lastText = text
			return
		}
		r.spinner.Message(text)
		if !r.running {
			_ = r.spinner.Start()
			r.running = true
		}
	case plog.StepEnd:
		if r.spinner == nil || !r.running {
			fmt.Fprintf(r.out, " ✓ %s\n", text)
			r.open = false
			return
		}
		r.spinner.StopMessage(text)
		_ = r.spinner.Stop()
		r.running = false
	default:
		// Ordinary log line: scroll it above the live spinner.
		r.printAboveSpinner(func() { fmt.Fprint(r.out, message) })
	}
}

// printAboveSpinner runs print so its output lands on a clean line above the
// live spinner. yacspin's Pause leaves its last animation frame on screen, so we
// erase that residual line (with yacspin's own erase sequence) before printing,
// then resume — without this, log lines concatenate onto the spinner line
// (" ⣽ Creating traffic managerLabeling Namespace ..."). Off a TTY, or with no
// running spinner, it just prints (no escape codes leak into pipes/files).
func (r *Renderer) printAboveSpinner(print func()) {
	if r.spinner != nil && r.running && smartTTY(r.out) {
		_ = r.spinner.Pause()
		fmt.Fprint(r.out, "\r\x1b[K") // erase the residual spinner frame
		print()
		_ = r.spinner.Unpause()
	} else {
		print()
	}
}

// Stop finalizes any in-flight step. A step still running at Stop never received
// its StepEnd (e.g. the connect failed mid-step), so it is marked failed (✗)
// rather than falsely reported as done.
func (r *Renderer) Stop() {
	if r.spinner != nil && r.running {
		_ = r.spinner.StopFail()
		r.running = false
		return
	}
	// Plain path: a step still open (never received StepEnd) means the run aborted
	// mid-step; mark it failed so it is not silently dropped. On success the last
	// StepEnd already cleared open, so nothing is printed here.
	if r.spinner == nil && r.open {
		fmt.Fprintf(r.out, " ✗ %s\n", r.lastText)
		r.open = false
	}
}
