// Package progress renders kind-style connect progress on the CLI: active steps
// animate as a spinner and finalize with a check mark, while ordinary log lines
// scroll above. The animation, TTY detection, Windows VT handling and non-TTY
// fallback are delegated to github.com/theckman/yacspin.
package progress

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/theckman/yacspin"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

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
		// Ordinary log line: scroll it above the live spinner, then resume.
		if r.spinner != nil && r.running {
			_ = r.spinner.Pause()
			fmt.Fprint(r.out, message)
			_ = r.spinner.Unpause()
		} else {
			fmt.Fprint(r.out, message)
		}
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
