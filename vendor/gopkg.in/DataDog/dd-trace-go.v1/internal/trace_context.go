// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package internal

import (
	"context"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/orchestrion"
)

type executionTracedKey struct{}

// WithExecutionTraced marks ctx as being associated with an execution trace
// task. It is assumed that ctx already contains a trace task. The caller is
// responsible for ending the task.
//
// This is intended for a specific case where the database/sql contrib package
// only creates spans *after* an operation, in case the operation was
// unavailable, and thus execution trace tasks tied to the span only capture the
// very end. This function enables creating a task *before* creating a span, and
// communicating to the APM tracer that it does not need to create a task. In
// general, APM instrumentation should prefer creating tasks around the
// operation rather than after the fact, if possible.
func WithExecutionTraced(ctx context.Context) context.Context {
	return orchestrion.CtxWithValue(ctx, executionTracedKey{}, true)
}

// WithExecutionNotTraced marks that the context is *not* covered by an
// execution trace task.  This is intended to prevent child spans (which inherit
// information from ctx) from being considered covered by a task, when an
// integration may create its own child span with its own execution trace task.
func WithExecutionNotTraced(ctx context.Context) context.Context {
	if orchestrion.WrapContext(ctx).Value(executionTracedKey{}) == nil {
		// Fast path: if it wasn't marked before, we don't need to wrap
		// the context
		return ctx
	}
	return orchestrion.CtxWithValue(ctx, executionTracedKey{}, false)
}

// IsExecutionTraced returns whether ctx is associated with an execution trace
// task, as indicated via WithExecutionTraced
func IsExecutionTraced(ctx context.Context) bool {
	v := orchestrion.WrapContext(ctx).Value(executionTracedKey{})
	return v != nil && v.(bool)
}
