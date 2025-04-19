// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package orchestrion

import (
	"context"
)

// WrapContext returns the GLS-wrapped context if orchestrion is enabled, otherwise it returns the given parameter.
func WrapContext(ctx context.Context) context.Context {
	if !Enabled() {
		return ctx
	}

	if _, ok := ctx.(*glsContext); ok { // avoid (some) double wrapping
		return ctx
	}

	if ctx == nil {
		ctx = context.Background()
	}

	return &glsContext{ctx}
}

// CtxWithValue runs context.WithValue, adds the result to the GLS slot of orchestrion, and returns it.
// If orchestrion is not enabled, it will run context.WithValue and return the result.
// Since we don't support cross-goroutine switch of the GLS we still run context.WithValue in the case
// we are switching goroutines.
func CtxWithValue(parent context.Context, key, val any) context.Context {
	if !Enabled() {
		return context.WithValue(parent, key, val)
	}

	getDDContextStack().Push(key, val)
	return context.WithValue(WrapContext(parent), key, val)
}

// GLSPopValue pops the value from the GLS slot of orchestrion and returns it. Using context.Context values usually does
// not require to pop any stack because the copy of each previous context makes the local variable in the scope disappear
// when the current function ends. But the GLS is a semi-global variable that can be accessed from any function in the
// stack, so we need to pop the value when we are done with it.
func GLSPopValue(key any) any {
	if !Enabled() {
		return nil
	}

	return getDDContextStack().Pop(key)
}

var _ context.Context = (*glsContext)(nil)

type glsContext struct {
	context.Context
}

func (g *glsContext) Value(key any) any {
	if !Enabled() {
		return g.Context.Value(key)
	}

	if val := getDDContextStack().Peek(key); val != nil {
		return val
	}

	return g.Context.Value(key)
}
