// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package graphqlsec

import (
	"context"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
)

type (
	ResolveOperation struct {
		dyngo.Operation
	}

	// ResolveOperationArgs describes arguments passed to a GraphQL field operation.
	ResolveOperationArgs struct {
		// TypeName is the name of the field's type
		TypeName string
		// FieldName is the name of the field
		FieldName string
		// Arguments is the arguments provided to the field resolver
		Arguments map[string]any
		// Trivial determines whether the resolution is trivial or not. Leave as false if undetermined.
		Trivial bool
	}

	ResolveOperationRes struct {
		// Data is the data returned from processing the GraphQL operation.
		Data any
		// Error is the error returned by processing the GraphQL Operation, if any.
		Error error
	}
)

// Finish the GraphQL Field operation, along with the given results, and emit a finish event up in
// the operation stack.
func (q *ResolveOperation) Finish(res ResolveOperationRes) {
	dyngo.FinishOperation(q, res)
}

func (ResolveOperationArgs) IsArgOf(*ResolveOperation)   {}
func (ResolveOperationRes) IsResultOf(*ResolveOperation) {}

// StartResolveOperation starts a new GraphQL Resolve operation, along with the given arguments, and
// emits a start event up in the operation stack. The operation is tracked on the returned context,
// and can be extracted later on using FromContext.
func StartResolveOperation(ctx context.Context, args ResolveOperationArgs) (context.Context, *ResolveOperation) {
	parent, ok := dyngo.FromContext(ctx)
	if !ok {
		log.Debug("appsec: StartResolveOperation: no parent operation found in context")
	}

	op := &ResolveOperation{
		Operation: dyngo.NewOperation(parent),
	}
	return dyngo.StartAndRegisterOperation(ctx, op, args), op
}
