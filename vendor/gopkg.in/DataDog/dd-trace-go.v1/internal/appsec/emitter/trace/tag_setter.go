// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package trace

// TagSetter is the interface needed to set a span tag.
type TagSetter interface {
	SetTag(string, any)
}

// NoopTagSetter is a TagSetter that does nothing. Useful when no tracer
// Span is available, but a TagSetter is assumed.
type NoopTagSetter struct{}

func (NoopTagSetter) SetTag(string, any) {
	// Do nothing
}

type TestTagSetter map[string]any

func (t TestTagSetter) SetTag(key string, value any) {
	t[key] = value
}

func (t TestTagSetter) Tags() map[string]any {
	return t
}
