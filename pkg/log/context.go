package log

import (
	"context"

	"github.com/sirupsen/logrus"
)

// G is a shorthand for [GetLogger].
//
// We may want to define this locally to a package to get package tagged log
// messages.
var G = GetLogger

// L is an alias for the standard logger.
var L = InitLoggerForServer()

type loggerKey struct{}

type loggerValue struct {
	logger *logrus.Logger
}

// WithLogger returns a new context with the provided logger. Use in
// combination with logger.WithField(s) for great effect.
func WithLogger(ctx context.Context, logger *logrus.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, &loggerValue{logger: logger})
}

func WithoutLogger(ctx context.Context) context.Context {
	if logger := ctx.Value(loggerKey{}); logger != nil {
		logger.(*loggerValue).logger = nil
	}
	return ctx
}

// GetLogger retrieves the current logger from the context. If no logger is
// available, the default logger is returned.
func GetLogger(ctx context.Context) *logrus.Logger {
	if logger := ctx.Value(loggerKey{}); logger != nil && logger.(*loggerValue).logger != nil {
		return logger.(*loggerValue).logger
	}
	return L
}
