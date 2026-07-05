package log

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	glog "gvisor.dev/gvisor/pkg/log"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetLogLevel returns the logrus level (as int32) implied by the --debug flag: DebugLevel when
// config.Debug is set, otherwise InfoLevel. CLI commands use it to populate the Level field of
// daemon RPC requests, so the daemon's StreamHook forwards logs back at the user-requested level.
func GetLogLevel() int32 {
	if config.Debug {
		return int32(log.DebugLevel)
	}
	return int32(log.InfoLevel)
}

// NewClientLogger creates a client-format logger writing to stdout.
// Used by CLI commands: cmd.SetContext(plog.WithLogger(ctx, plog.NewClientLogger()))
func NewClientLogger() *log.Logger {
	return GetLoggerForClient(GetLogLevel(), os.Stdout)
}

// GetLoggerForClient returns a new logger configured for client-side use
// (message-only format, no timestamp/file:line). Used by CLI commands.
func GetLoggerForClient(level int32, out io.Writer) *log.Logger {
	return &log.Logger{
		Out:          out,
		Formatter:    &format{},
		Hooks:        make(log.LevelHooks),
		Level:        log.Level(level),
		ExitFunc:     os.Exit,
		ReportCaller: false,
	}
}

// GetLoggerForServer returns a new logger configured for server-side use
// (timestamp + file:line + level). Used by daemon RPC handlers.
func GetLoggerForServer(level int32, out io.Writer) *log.Logger {
	return &log.Logger{
		Out:          out,
		Formatter:    &serverFormat{},
		Hooks:        make(log.LevelHooks),
		Level:        log.Level(level),
		ExitFunc:     os.Exit,
		ReportCaller: true,
	}
}

// InitLoggerForServer returns the default server-format logger writing to stderr
// at InfoLevel. The daemon upgrades to DebugLevel after redirecting output to log file.
func InitLoggerForServer() *log.Logger {
	return GetLoggerForServer(int32(log.InfoLevel), os.Stderr)
}

const (
	// stepFieldKey marks a log entry as a CLI progress step. It is consumed by
	// StreamHook (encoded as a stream sentinel for the spinner renderer) and is
	// never rendered into the daemon log file (serverFormat strips it).
	stepFieldKey = "_kubevpn_step"
	stepStart    = "start"
	stepDone     = "done"
	stepTitle    = "title"

	// stepSentinelStart/Done/Title prefix a streamed message to tell the CLI
	// renderer to begin a spinner / finalize it with a check mark / print it as a
	// bold heading. They use the ASCII Unit Separator (U+001F) — valid UTF-8 that
	// never appears in normal messages.
	stepSentinelStart = "\x1fS"
	stepSentinelDone  = "\x1fD"
	stepSentinelTitle = "\x1fT"
)

// StepKind classifies a streamed message for the CLI progress renderer.
type StepKind int

const (
	// StepNone is an ordinary log line (no spinner involvement).
	StepNone StepKind = iota
	// StepBegin starts/updates the active spinner line.
	StepBegin
	// StepEnd finalizes the active spinner line with a check mark.
	StepEnd
	// StepHeading is a bold heading line (no spinner, no check mark) — e.g. the
	// "Connecting to the cluster ..." banner that precedes the steps. Emitted by
	// the StepTitle helper.
	StepHeading
)

// EncodeStep prefixes a message with the step sentinel for the given kind so the
// CLI renderer can recognize it. StepNone returns the message unchanged.
func EncodeStep(kind StepKind, message string) string {
	switch kind {
	case StepBegin:
		return stepSentinelStart + message
	case StepEnd:
		return stepSentinelDone + message
	case StepHeading:
		return stepSentinelTitle + message
	default:
		return message
	}
}

// DecodeStep inspects a streamed message, strips any step sentinel, and reports
// its kind. The CLI progress renderer uses this to drive the spinner without
// hard-coding the wire encoding.
func DecodeStep(message string) (StepKind, string) {
	switch {
	case strings.HasPrefix(message, stepSentinelStart):
		return StepBegin, message[len(stepSentinelStart):]
	case strings.HasPrefix(message, stepSentinelDone):
		return StepEnd, message[len(stepSentinelDone):]
	case strings.HasPrefix(message, stepSentinelTitle):
		return StepHeading, message[len(stepSentinelTitle):]
	default:
		return StepNone, message
	}
}

// StreamHook sends message-only text to a writer (typically a gRPC stream).
// Attach to a server-format logger so the primary output (log file) gets full
// debug info while the stream gets clean user-facing messages.
type StreamHook struct {
	Writer io.Writer
	Level  log.Level
}

func (h *StreamHook) Levels() []log.Level {
	var levels []log.Level
	for _, l := range log.AllLevels {
		if l <= h.Level {
			levels = append(levels, l)
		}
	}
	return levels
}

func (h *StreamHook) Fire(entry *log.Entry) error {
	msg := entry.Message
	switch entry.Data[stepFieldKey] {
	case stepStart:
		msg = EncodeStep(StepBegin, msg)
	case stepDone:
		msg = EncodeStep(StepEnd, msg)
	case stepTitle:
		msg = EncodeStep(StepHeading, msg)
	}
	_, err := h.Writer.Write([]byte(msg + "\n"))
	return err
}

type format struct{}

// Format outputs only the message text followed by a newline.
func (*format) Format(e *log.Entry) ([]byte, error) {
	return []byte(e.Message + "\n"), nil
}

type serverFormat struct{}

// Format
// same like log.SetFlags(log.LstdFlags | log.Lshortfile)
// 2009/01/23 01:23:23 d.go:23: message
func (*serverFormat) Format(e *log.Entry) ([]byte, error) {
	// Drop internal-only fields (e.g. the CLI step marker) so the log file stays
	// clean; they are meant for the stream sentinel, not the on-disk record.
	data := e.Data
	if _, ok := data[stepFieldKey]; ok {
		data = make(log.Fields, len(e.Data))
		for k, v := range e.Data {
			if k == stepFieldKey {
				continue
			}
			data[k] = v
		}
	}
	// e.Caller maybe is nil, because pkg/handler/connect.go:252
	if len(data) > 0 {
		return []byte(
			fmt.Sprintf("%s %s %s:%d %s: %s\n",
				GenStr(data),
				e.Time.Format("2006-01-02 15:04:05.000"),
				filepath.Base(ptr.Deref(e.Caller, runtime.Frame{}).File),
				ptr.Deref(e.Caller, runtime.Frame{}).Line,
				e.Level.String(),
				e.Message,
			)), nil
	}

	return []byte(
		fmt.Sprintf("%s %s:%d %s: %s\n",
			e.Time.Format("2006-01-02 15:04:05.000"),
			filepath.Base(ptr.Deref(e.Caller, runtime.Frame{}).File),
			ptr.Deref(e.Caller, runtime.Frame{}).Line,
			e.Level.String(),
			e.Message,
		)), nil
}

// ServerEmitter adapts gvisor's log.Emitter interface to write server-formatted log output.
type ServerEmitter struct {
	*glog.Writer
}

func (g ServerEmitter) Emit(depth int, level glog.Level, timestamp time.Time, format string, args ...any) {
	_, file, line, ok := runtime.Caller(depth + 2)
	if !ok {
		file = "???"
		line = 0
	} else {
		file = filepath.Base(file)
	}

	// Generate the message.
	message := fmt.Sprintf(format, args...)

	// Emit the formatted result.
	_, _ = fmt.Fprintf(g.Writer, "%s %s:%d %s: %s\n",
		timestamp.Format("2006-01-02 15:04:05.000"),
		file,
		line,
		level.String(),
		message,
	)
}

// GenStr formats a map of log fields into a sorted bracketed key=value string.
func GenStr(allFields map[string]any) string {
	keys := slices.Sorted(maps.Keys(allFields))

	var b strings.Builder
	b.WriteByte('[')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}

		var valueStr string
		if stringer, ok := allFields[key].(fmt.Stringer); ok {
			valueStr = stringer.String()
		} else {
			valueStr = fmt.Sprintf("%v", allFields[key])
		}

		if strings.Contains(valueStr, " ") {
			valueStr = `"` + valueStr + `"`
		}
		b.WriteString(key)
		if valueStr != "" {
			b.WriteByte('=')
			b.WriteString(valueStr)
		}
	}
	b.WriteByte(']')
	return b.String()
}
