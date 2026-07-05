// Package progress renders kind-style connect progress on the CLI: active steps
// animate as a spinner and finalize with a check mark, while ordinary log lines
// scroll above. The animation, TTY detection, Windows VT handling and non-TTY
// fallback are delegated to github.com/theckman/yacspin.
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

// ANSI styles for the non-spinner lines. yacspin already emits raw ANSI for the
// step check marks (its writer is os.Stdout, not a colorable wrapper), so the
// heading and success line use raw ANSI too — consistent rendering on every
// terminal where the check marks already show in color.
const (
	ansiBold      = "\x1b[1m"
	ansiBoldGreen = "\x1b[1;32m"
	ansiReset     = "\x1b[0m"
)

// isTerminal reports whether w is an interactive terminal. Non-terminal writers
// (pipes, files, test buffers) get plain, un-styled output.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// styleLine writes msg + newline wrapped in the ANSI code on a terminal, plain
// otherwise.
func styleLine(w io.Writer, code, msg string) {
	if isTerminal(w) {
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
}

// New returns a Renderer writing to out. yacspin auto-detects whether out is a
// terminal and degrades to non-animated, line-by-line output otherwise.
func New(out io.Writer) *Renderer {
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
			return // matching StepEnd prints the line
		}
		r.spinner.Message(text)
		if !r.running {
			_ = r.spinner.Start()
			r.running = true
		}
	case plog.StepEnd:
		if r.spinner == nil || !r.running {
			fmt.Fprintf(r.out, " ✓ %s\n", text)
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
	if r.spinner != nil && r.running && isTerminal(r.out) {
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
	}
}
