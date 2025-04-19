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

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
)

type (
	ExecutionOperation struct {
		dyngo.Operation
	}

	// ExecutionOperationArgs describes arguments passed to a GraphQL query operation.
	ExecutionOperationArgs struct {
		// Variables is the user-provided variables object for the query.
		Variables map[string]any
		// Query is the query that is being executed.
		Query string
		// OperationName is the user-provided operation name for the query.
		OperationName string
	}

	ExecutionOperationRes struct {
		// Data is the data returned from processing the GraphQL operation.
		Data any
		// Error is the error returned by processing the GraphQL Operation, if any.
		Error error
	}
)

// Finish the GraphQL query operation, along with the given results, and emit a finish event up in
// the operation stack.
func (q *ExecutionOperation) Finish(res ExecutionOperationRes) {
	dyngo.FinishOperation(q, res)
}

func (ExecutionOperationArgs) IsArgOf(*ExecutionOperation)   {}
func (ExecutionOperationRes) IsResultOf(*ExecutionOperation) {}

// StartExecutionOperation starts a new GraphQL query operation, along with the given arguments, and
// emits a start event up in the operation stack. The operation is tracked on the returned context,
// and can be extracted later on using FromContext.
func StartExecutionOperation(ctx context.Context, args ExecutionOperationArgs) (context.Context, *ExecutionOperation) {
	parent, ok := dyngo.FromContext(ctx)
	if !ok {
		log.Debug("appsec: StartExecutionOperation: no parent operation found in context")
	}

	op := &ExecutionOperation{
		Operation: dyngo.NewOperation(parent),
	}

	return dyngo.StartAndRegisterOperation(ctx, op, args), op
}
