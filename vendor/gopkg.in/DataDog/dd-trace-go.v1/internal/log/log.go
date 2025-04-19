// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package log provides logging utilities for the tracer.
package log

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/version"
)

// Level specifies the logging level that the log package prints at.
type Level int

const (
	// LevelDebug represents debug level messages.
	LevelDebug Level = iota
	// LevelWarn represents warning and errors.
	LevelWarn
)

var prefixMsg = fmt.Sprintf("Datadog Tracer %s", version.Tag)

// Logger implementations are able to log given messages that the tracer might
// output. This interface is duplicated here to avoid a cyclic dependency
// between this package and ddtrace
type Logger interface {
	// Log prints the given message.
	Log(msg string)
}

// File name for writing tracer logs, if DD_TRACE_LOG_DIRECTORY has been configured
const LoggerFile = "ddtrace.log"

// ManagedFile functions like a *os.File but is safe for concurrent use
type ManagedFile struct {
	mu     sync.RWMutex
	file   *os.File
	closed bool
}

// Close closes the ManagedFile's *os.File in a concurrent-safe manner, ensuring the file is closed only once
func (m *ManagedFile) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.file == nil || m.closed {
		return nil
	}
	err := m.file.Close()
	if err != nil {
		return err
	}
	m.closed = true
	return nil
}

func (m *ManagedFile) Name() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.file == nil {
		return ""
	}
	return m.file.Name()
}

var (
	mu     sync.RWMutex // guards below fields
	level               = LevelWarn
	logger Logger       = &defaultLogger{l: log.New(os.Stderr, "", log.LstdFlags)}
)

// UseLogger sets l as the active logger and returns a function to restore the
// previous logger. The return value is mostly useful when testing.
func UseLogger(l Logger) (undo func()) {
	Flush()
	mu.Lock()
	defer mu.Unlock()
	old := logger
	logger = l
	return func() {
		logger = old
	}
}

// OpenFileAtPath creates a new file at the specified dirPath and configures the logger to write to this file. The dirPath must already exist on the underlying os.
// It returns the file that was created, or nil and an error if the file creation was unsuccessful.
// The caller of OpenFileAtPath is responsible for calling Close() on the ManagedFile
func OpenFileAtPath(dirPath string) (*ManagedFile, error) {
	path, err := os.Stat(dirPath)
	if err != nil || !path.IsDir() {
		return nil, fmt.Errorf("file path %v invalid or does not exist on the underlying os; using default logger to stderr", dirPath)
	}
	filepath := dirPath + "/" + LoggerFile
	f, err := os.OpenFile(filepath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		return nil, fmt.Errorf("using default logger to stderr due to error creating or opening log file: %v", err)
	}
	UseLogger(&defaultLogger{l: log.New(f, "", log.LstdFlags)})
	return &ManagedFile{
		file: f,
	}, nil
}

// SetLevel sets the given lvl for logging.
func SetLevel(lvl Level) {
	mu.Lock()
	defer mu.Unlock()
	level = lvl
}

// GetLevel returns the currrent log level.
func GetLevel() Level {
	mu.Lock()
	defer mu.Unlock()
	return level
}

// DebugEnabled returns true if debug log messages are enabled. This can be used in extremely
// hot code paths to avoid allocating the ...interface{} argument.
func DebugEnabled() bool {
	mu.RLock()
	lvl := level
	mu.RUnlock()
	return lvl == LevelDebug
}

// Debug prints the given message if the level is LevelDebug.
func Debug(fmt string, a ...interface{}) {
	if !DebugEnabled() {
		return
	}
	printMsg("DEBUG", fmt, a...)
}

// Warn prints a warning message.
func Warn(fmt string, a ...interface{}) {
	printMsg("WARN", fmt, a...)
}

// Info prints an informational message.
func Info(fmt string, a ...interface{}) {
	printMsg("INFO", fmt, a...)
}

var (
	errmu   sync.RWMutex                // guards below fields
	erragg  = map[string]*errorReport{} // aggregated errors
	errrate = time.Minute               // the rate at which errors are reported
	erron   bool                        // true if errors are being aggregated
)

func init() {
	if v := os.Getenv("DD_LOGGING_RATE"); v != "" {
		setLoggingRate(v)
	}

	// This is required because we really want to be able to log errors from dyngo
	// but the log package depend on too much packages that we want to instrument.
	// So we need to do this to avoid dependency cycles.
	dyngo.LogError = Error
}

func setLoggingRate(v string) {
	if sec, err := strconv.ParseInt(v, 10, 64); err != nil {
		Warn("Invalid value for DD_LOGGING_RATE: %v", err)
	} else {
		if sec < 0 {
			Warn("Invalid value for DD_LOGGING_RATE: negative value")
		} else {
			// DD_LOGGING_RATE = 0 allows to log errors immediately.
			errrate = time.Duration(sec) * time.Second
		}
	}
}

type errorReport struct {
	first time.Time // time when first error occurred
	err   error
	count uint64
}

// Error reports an error. Errors get aggregated and logged periodically. The
// default is once per minute or once every DD_LOGGING_RATE number of seconds.
func Error(format string, a ...interface{}) {
	key := format // format should 99.9% of the time be constant
	if reachedLimit(key) {
		// avoid too much lock contention on spammy errors
		return
	}
	errmu.Lock()
	defer errmu.Unlock()
	report, ok := erragg[key]
	if !ok {
		erragg[key] = &errorReport{
			err:   fmt.Errorf(format, a...),
			first: time.Now(),
		}
		report = erragg[key]
	}
	report.count++
	if errrate == 0 {
		flushLocked()
		return
	}
	if !erron {
		erron = true
		time.AfterFunc(errrate, Flush)
	}
}

// defaultErrorLimit specifies the maximum number of errors gathered in a report.
const defaultErrorLimit = 200

// reachedLimit reports whether the maximum count has been reached for this key.
func reachedLimit(key string) bool {
	errmu.RLock()
	e, ok := erragg[key]
	confirm := ok && e.count > defaultErrorLimit
	errmu.RUnlock()
	return confirm
}

// Flush flushes and resets all aggregated errors to the logger.
func Flush() {
	errmu.Lock()
	defer errmu.Unlock()
	flushLocked()
}

func flushLocked() {
	for _, report := range erragg {
		var extra string
		if report.count > defaultErrorLimit {
			extra = fmt.Sprintf(", %d+ additional messages skipped (first occurrence: %s)", defaultErrorLimit, report.first.Format(time.RFC822))
		} else if report.count > 1 {
			extra = fmt.Sprintf(", %d additional messages skipped (first occurrence: %s)", report.count-1, report.first.Format(time.RFC822))
		} else {
			extra = fmt.Sprintf(" (occurred: %s)", report.first.Format(time.RFC822))
		}
		printMsg("ERROR", "%v%s", report.err, extra)
	}
	for k := range erragg {
		// compiler-optimized map-clearing post go1.11 (golang/go#20138)
		delete(erragg, k)
	}
	erron = false
}

func printMsg(lvl, format string, a ...interface{}) {
	msg := fmt.Sprintf("%s %s: %s", prefixMsg, lvl, fmt.Sprintf(format, a...))
	mu.RLock()
	logger.Log(msg)
	mu.RUnlock()
}

type defaultLogger struct{ l *log.Logger }

func (p *defaultLogger) Log(msg string) { p.l.Print(msg) }

// DiscardLogger discards every call to Log().
type DiscardLogger struct{}

// Log implements Logger.
func (d DiscardLogger) Log(_ string) {}

// RecordLogger records every call to Log() and makes it available via Logs().
type RecordLogger struct {
	m      sync.Mutex
	logs   []string
	ignore []string // a log is ignored if it contains a string in ignored
}

// Ignore adds substrings to the ignore field of RecordLogger, allowing
// the RecordLogger to ignore attempts to log strings with certain substrings.
func (r *RecordLogger) Ignore(substrings ...string) {
	r.m.Lock()
	defer r.m.Unlock()
	r.ignore = append(r.ignore, substrings...)
}

// Log implements Logger.
func (r *RecordLogger) Log(msg string) {
	r.m.Lock()
	defer r.m.Unlock()
	for _, ignored := range r.ignore {
		if strings.Contains(msg, ignored) {
			return
		}
	}
	r.logs = append(r.logs, msg)
}

// Logs returns the ordered list of logs recorded by the logger.
func (r *RecordLogger) Logs() []string {
	r.m.Lock()
	defer r.m.Unlock()
	copied := make([]string, len(r.logs))
	copy(copied, r.logs)
	return copied
}

// Reset resets the logger's internal logs
func (r *RecordLogger) Reset() {
	r.m.Lock()
	defer r.m.Unlock()
	r.logs = r.logs[:0]
	r.ignore = r.ignore[:0]
}
