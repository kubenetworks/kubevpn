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

// NewClientLogger creates a client-format logger writing to stdout.
// Used by CLI commands: cmd.SetContext(plog.WithLogger(ctx, plog.NewClientLogger()))
func NewClientLogger() *log.Logger {
	level := log.InfoLevel
	if config.Debug {
		level = log.DebugLevel
	}
	return GetLoggerForClient(int32(level), os.Stdout)
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
	_, err := h.Writer.Write([]byte(entry.Message + "\n"))
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
	// e.Caller maybe is nil, because pkg/handler/connect.go:252
	if len(e.Data) > 0 {
		return []byte(
			fmt.Sprintf("%s %s %s:%d %s: %s\n",
				GenStr(e.Data),
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
