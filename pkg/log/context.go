package log

import (
	"context"

	log "github.com/sirupsen/logrus"
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
	logger *log.Logger
}

// WithLogger returns a new context with the provided logger. Use in
// combination with logger.WithField(s) for great effect.
func WithLogger(ctx context.Context, logger *log.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, &loggerValue{logger: logger})
}

func WithoutLogger(ctx context.Context) context.Context {
	if logger := ctx.Value(loggerKey{}); logger != nil {
		logger.(*loggerValue).logger = nil
	}
	return ctx
}

// getLogger retrieves the current logger from the context. If no logger is
// available, the default logger is returned.
func getLogger(ctx context.Context) *log.Logger {
	if logger := ctx.Value(loggerKey{}); logger != nil && logger.(*loggerValue).logger != nil {
		return logger.(*loggerValue).logger
	}
	return L
}

type fieldsKey struct{}

// WithFields 将指定的字段添加到 context 中，这些字段会在后续从 context 获取 logger 时自动添加
func WithFields(ctx context.Context, fields map[string]any) context.Context {
	existingFields := GetFields(ctx)
	if existingFields == nil {
		return context.WithValue(ctx, fieldsKey{}, fields)
	}

	// 合并字段，新字段会覆盖旧字段
	mergedFields := make(map[string]any)
	for k, v := range existingFields {
		mergedFields[k] = v
	}
	for k, v := range fields {
		mergedFields[k] = v
	}

	return context.WithValue(ctx, fieldsKey{}, mergedFields)
}

// WithField 将单个字段添加到 context 中
func WithField(ctx context.Context, key string, value any) context.Context {
	return WithFields(ctx, map[string]any{key: value})
}

// GetFields 从 context 中获取所有已存储的字段
func GetFields(ctx context.Context) map[string]any {
	if fields := ctx.Value(fieldsKey{}); fields != nil {
		if f, ok := fields.(map[string]any); ok {
			return f
		}
	}
	return nil
}

// GetLogger 从 context 中获取 logger，并自动添加 context 中存储的字段
func GetLogger(ctx context.Context) *log.Entry {
	logger := getLogger(ctx)
	fields := GetFields(ctx)

	if len(fields) > 0 {
		return logger.WithFields(fields)
	}

	return log.NewEntry(logger)
}
