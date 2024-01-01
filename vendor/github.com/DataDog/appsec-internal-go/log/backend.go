// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package log

import (
	"fmt"
	"log"
	"os"
	"strings"
)

var (
	backend = Backend{
		Trace:     defaultWithLevel(logLevelTrace),
		Debug:     defaultWithLevel(logLevelDebug),
		Info:      defaultWithLevel(logLevelInfo),
		Warn:      defaultWithLevel(logLevelWarn),
		Errorf:    defaultErrorfWithLevel(logLevelError),
		Criticalf: defaultErrorfWithLevel(logLevelCritical),
	}
	defaultBackendLogLevel = logLevelError
)

type Backend struct {
	Trace     func(string, ...any)
	Debug     func(string, ...any)
	Info      func(string, ...any)
	Warn      func(string, ...any)
	Errorf    func(string, ...any) error
	Criticalf func(string, ...any) error
}

// SetBackend replaces the active log backend with the provided one. Any nil
// function in the new backend will silently ignore any message logged at that
// level.
func SetBackend(newBackend Backend) {
	if newBackend.Trace == nil {
		newBackend.Trace = noopLogger
	}
	if newBackend.Debug == nil {
		newBackend.Debug = noopLogger
	}
	if newBackend.Info == nil {
		newBackend.Info = noopLogger
	}
	if newBackend.Warn == nil {
		newBackend.Warn = noopLogger
	}
	if newBackend.Errorf == nil {
		newBackend.Errorf = fmt.Errorf
	}
	if newBackend.Criticalf == nil {
		newBackend.Criticalf = fmt.Errorf
	}

	backend = newBackend
}

// defaultWithLevel returns the default log backend function for the provided
// logLevel. This returns a no-op function if the default backend logLevel does
// not enable logging at that level.
func defaultWithLevel(level logLevel) func(string, ...any) {
	if defaultBackendLogLevel < level {
		return noopLogger
	}
	return func(format string, args ...any) {
		log.Printf(fmt.Sprintf("[%s] %s\n", level, format), args...)
	}
}

// defaultErrorfWithLevel returns the default log backend function for the
// provided error logLevel.
func defaultErrorfWithLevel(level logLevel) func(string, ...any) error {
	if defaultBackendLogLevel < level {
		return fmt.Errorf
	}
	return func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		log.Printf("[%s] %v", level, err)
		return err
	}
}

// noopLogger does nothing.
func noopLogger(string, ...any) { /* noop */ }

type logLevel uint8

const (
	logLevelTrace logLevel = 1 << iota
	logLevelDebug
	logLevelInfo
	logLevelWarn
	logLevelError
	logLevelCritical
)

func (l logLevel) String() string {
	switch l {
	case logLevelTrace:
		return "TRACE"
	case logLevelDebug:
		return "DEBUG"
	case logLevelInfo:
		return "INFO"
	case logLevelWarn:
		return "WARN"
	case logLevelError:
		return "ERROR"
	case logLevelCritical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

func init() {
	ddLogLevel := os.Getenv("DD_LOG_LEVEL")
	switch strings.ToUpper(ddLogLevel) {
	case "TRACE":
		defaultBackendLogLevel = logLevelTrace
	case "DEBUG":
		defaultBackendLogLevel = logLevelDebug
	case "INFO":
		defaultBackendLogLevel = logLevelInfo
	case "WARN":
		defaultBackendLogLevel = logLevelWarn
	case "ERROR":
		defaultBackendLogLevel = logLevelError
	case "CRITICAL":
		defaultBackendLogLevel = logLevelCritical
	default:
		// Ignore invalid/unexpected values
	}
}
