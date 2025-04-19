// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package grpcsec is the gRPC instrumentation API and contract for AppSec
// defining an abstract run-time representation of gRPC handlers.
// gRPC integrations must use this package to enable AppSec features for gRPC,
// which listens to this package's operation events.
//
// Abstract gRPC server handler operation definitions. It is based on two
// operations allowing to describe every type of RPC: the HandlerOperation type
// which represents the RPC handler, and the ReceiveOperation type which
// represents the messages the RPC handler receives during its lifetime.
// This means that the ReceiveOperation(s) will happen within the
// HandlerOperation.
// Every type of RPC, unary, client streaming, server streaming, and
// bidirectional streaming RPCs, can be all represented with a HandlerOperation
// having one or several ReceiveOperation.
// The send operation is not required for now and therefore not defined, which
// means that server and bidirectional streaming RPCs currently have the same
// run-time representation as unary and client streaming RPCs.
package grpcsec

import (
	"context"
	"sync/atomic"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/trace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf/actions"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter/waf/addresses"
)

type (
	// HandlerOperation represents a gRPC server handler operation.
	// It must be created with StartHandlerOperation() and finished with its
	// Finish() method.
	// Security events observed during the operation lifetime should be added
	// to the operation using its AddSecurityEvent() method.
	HandlerOperation struct {
		dyngo.Operation
		*waf.ContextOperation

		// wafContextOwner indicates if the waf.ContextOperation was started by us or not and if we need to close it.
		wafContextOwner bool
	}

	// HandlerOperationArgs is the grpc handler arguments.
	HandlerOperationArgs struct {
		// Method is the gRPC method name.
		// Corresponds to the address `grpc.server.method`.
		Method string

		// RPC metadata received by the gRPC handler.
		// Corresponds to the address `grpc.server.request.metadata`.
		Metadata map[string][]string

		// RemoteAddr is the IP address of the client that initiated the gRPC request.
		// May be used as the address `http.client_ip`.
		RemoteAddr string
	}

	// HandlerOperationRes is the grpc handler results. Empty as of today.
	HandlerOperationRes struct {
		// Raw gRPC status code.
		// Corresponds to the address `grpc.server.response.status`.
		StatusCode int
	}
)

func (HandlerOperationArgs) IsArgOf(*HandlerOperation)   {}
func (HandlerOperationRes) IsResultOf(*HandlerOperation) {}

// StartHandlerOperation starts an gRPC server handler operation, along with the
// given arguments and parent operation, and emits a start event up in the
// operation stack. When parent is nil, the operation is linked to the global
// root operation.
func StartHandlerOperation(ctx context.Context, span trace.TagSetter, args HandlerOperationArgs) (context.Context, *HandlerOperation, *atomic.Pointer[actions.BlockGRPC]) {
	wafOp, found := dyngo.FindOperation[waf.ContextOperation](ctx)
	if !found {
		wafOp, ctx = waf.StartContextOperation(ctx, span)
	}
	op := &HandlerOperation{
		Operation:        dyngo.NewOperation(wafOp),
		ContextOperation: wafOp,
		wafContextOwner:  !found, // If the parent is not found, we need to close the WAF context.
	}

	var block atomic.Pointer[actions.BlockGRPC]
	dyngo.OnData(op, func(err *actions.BlockGRPC) {
		block.Store(err)
	})

	return dyngo.StartAndRegisterOperation(ctx, op, args), op, &block
}

// MonitorRequestMessage monitors the gRPC request message body as the WAF address `grpc.server.request.message`.
func MonitorRequestMessage(ctx context.Context, msg any) error {
	return waf.RunSimple(ctx,
		addresses.NewAddressesBuilder().
			WithGRPCRequestMessage(msg).
			Build(),
		"appsec: failed to monitor gRPC request message body")
}

// MonitorResponseMessage monitors the gRPC response message body as the WAF address `grpc.server.response.message`.
func MonitorResponseMessage(ctx context.Context, msg any) error {
	return waf.RunSimple(ctx,
		addresses.NewAddressesBuilder().
			WithGRPCResponseMessage(msg).
			Build(),
		"appsec: failed to monitor gRPC response message body")

}

// Finish the gRPC handler operation, along with the given results, and emit a
// finish event up in the operation stack.
func (op *HandlerOperation) Finish(res HandlerOperationRes) {
	dyngo.FinishOperation(op, res)
	if op.wafContextOwner {
		op.ContextOperation.Finish()
	}
}
