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

// InitLoggerForClient configures the package-level logger L for client-side use with stdout output.
func InitLoggerForClient() {
	level := log.InfoLevel
	if config.Debug {
		level = log.DebugLevel
	}
	L = GetLoggerForClient(int32(level), os.Stdout)
}

// GetLoggerForClient returns a new logger configured for client-side use at the given level and output writer.
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

// InitLoggerForServer returns a new logger configured for server-side use with caller info and stderr output.
func InitLoggerForServer() *log.Logger {
	return &log.Logger{
		Out:          os.Stderr,
		Formatter:    &serverFormat{},
		Hooks:        make(log.LevelHooks),
		Level:        log.DebugLevel,
		ExitFunc:     os.Exit,
		ReportCaller: true,
	}
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
