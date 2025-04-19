// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package graphql is the GraphQL instrumentation API and contract for AppSec
// defining an abstract run-time representation of AppSec middleware. GraphQL
// integrations must use this package to enable AppSec features for GraphQL,
// which listens to this package's operation events.
package graphqlsec

import (
	"context"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/trace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf"
)

type (
	RequestOperation struct {
		dyngo.Operation
		// used in case we don't have a parent operation
		*waf.ContextOperation

		// wafContextOwner indicates if the waf.ContextOperation was started by us or not and if we need to close it.
		wafContextOwner bool
	}

	// RequestOperationArgs describes arguments passed to a GraphQL request.
	RequestOperationArgs struct {
		RawQuery      string         // The raw, not-yet-parsed GraphQL query
		OperationName string         // The user-provided operation name for the query
		Variables     map[string]any // The user-provided variables object for this request
	}

	RequestOperationRes struct {
		// Data is the data returned from processing the GraphQL operation.
		Data any
		// Error is the error returned by processing the GraphQL Operation, if any.
		Error error
	}
)

// Finish the GraphQL query operation, along with the given results, and emit a finish event up in
// the operation stack.
func (op *RequestOperation) Finish(res RequestOperationRes) {
	dyngo.FinishOperation(op, res)
	if op.wafContextOwner {
		op.ContextOperation.Finish()
	}
}

func (RequestOperationArgs) IsArgOf(*RequestOperation)   {}
func (RequestOperationRes) IsResultOf(*RequestOperation) {}

// StartRequestOperation starts a new GraphQL request operation, along with the given arguments, and
// emits a start event up in the operation stack. The operation is usually linked to tge global root
// operation. The operation is tracked on the returned context, and can be extracted later on using
// FromContext.
func StartRequestOperation(ctx context.Context, span trace.TagSetter, args RequestOperationArgs) (context.Context, *RequestOperation) {
	wafOp, found := dyngo.FindOperation[waf.ContextOperation](ctx)
	if !found { // Usually we can find the HTTP Handler Operation as the parent, but it's technically optional
		wafOp, ctx = waf.StartContextOperation(ctx, span)
	}

	op := &RequestOperation{
		Operation:        dyngo.NewOperation(wafOp),
		ContextOperation: wafOp,
		wafContextOwner:  !found, // If we started the parent operation, we finish it, otherwise we don't
	}

	return dyngo.StartAndRegisterOperation(ctx, op, args), op
}
