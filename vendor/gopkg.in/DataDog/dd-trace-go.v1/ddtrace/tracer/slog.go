// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package tracer

import (
	"context"
	"log/slog"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// slogHandler implements the slog.Handler interface to dispatch messages to our
// internal logger.
type slogHandler struct {
	attrs  []string
	groups []string
}

func (h slogHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	if lvl <= slog.LevelDebug {
		return log.DebugEnabled()
	}
	// TODO(fg): Implement generic log level checking in the internal logger.
	// But we're we're not concerned with slog perf, so this is okay for now.
	return true
}

func (h slogHandler) Handle(ctx context.Context, r slog.Record) error {
	parts := make([]string, 0, len(h.attrs)+r.NumAttrs())
	parts = append(parts, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, formatAttr(a, h.groups))
		return true
	})

	extra := strings.Join(parts, " ")
	switch r.Level {
	case slog.LevelDebug:
		log.Debug("%s %s", r.Message, extra)
	case slog.LevelInfo:
		log.Info("%s %s", r.Message, extra)
	case slog.LevelWarn:
		log.Warn("%s %s", r.Message, extra)
	case slog.LevelError:
		log.Error("%s %s", r.Message, extra)
	}
	return nil
}

func (h slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	for _, a := range attrs {
		h.attrs = append(h.attrs, formatAttr(a, h.groups))
	}
	return h
}

func (h slogHandler) WithGroup(name string) slog.Handler {
	h.groups = append(h.groups, name)
	return h
}

func formatAttr(a slog.Attr, groups []string) string {
	return strings.Join(append(groups, a.String()), ".")
}
