// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

// Package log provides a logging facility that is used by this library, and
// which can be configured to piggyback on another logging facility where
// available. If not explicitly configured, this will log messages using the Go
// standar library log package, filtered according to the log level set in the
// `DD_LOG_LEVEL` environment variable (or `ERROR` if none is set).
//
// Custom logger intergrations are configured by calling the SetBackend function.
package log

// Trace logs a message with format using the TRACE log level.
func Trace(format string, args ...any) {
	backend.Trace(format, args...)
}

// Debug logs a message with format using the DEBUG log level.
func Debug(format string, args ...any) {
	backend.Debug(format, args...)
}

// Info logs a message with format using the INFO log level.
func Info(format string, args ...any) {
	backend.Info(format, args...)
}

// Warn logs a message with format using the WARN log level.
func Warn(format string, args ...any) {
	backend.Warn(format, args...)
}

// Errorf logs a message with format using the ERROR log level and returns an
// error containing the formatted log message.
func Errorf(format string, args ...any) error {
	return backend.Errorf(format, args...)
}

// Errorf logs a message with format using the CRITICAL log level and returns an
// error containing the formatted log message.
func Criticalf(format string, args ...any) error {
	return backend.Criticalf(format, args...)
}
