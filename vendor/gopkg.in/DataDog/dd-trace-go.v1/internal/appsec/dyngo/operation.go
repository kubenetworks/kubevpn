// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package dyngo is the Go implementation of Datadog's Instrumentation Gateway
// which provides an event-based instrumentation API based on a stack
// representation of instrumented functions along with nested event listeners.
// It allows to both correlate passed and future function calls in order to
// react and monitor specific function call scenarios, while keeping the
// monitoring state local to the monitoring logic thanks to nested Go function
// closures.
// dyngo is not intended to be directly used and should be instead wrapped
// behind statically and strongly typed wrapper types. Indeed, dyngo is a
// generic implementation relying on empty interface values (values of type
// `interface{}`) and using it directly can be error-prone due to the lack of
// compile-time type-checking. For example, AppSec provides the package
// `httpsec`, built on top of dyngo, as its HTTP instrumentation API and which
// defines the abstract HTTP operation representation expected by the AppSec
// monitoring.
package dyngo

import (
	"context"
	"sync"
	"sync/atomic"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/orchestrion"
)

// LogError is the function used to log errors in the dyngo package.
// This is required because we really want to be able to log errors from dyngo
// but the log package depend on too much packages that we want to instrument.
// So we need to do this to avoid dependency cycles.
var LogError = func(string, ...any) {}

// Operation interface type allowing to register event listeners to the
// operation. The event listeners will be automatically removed from the
// operation once it finishes so that it no longer can be called on finished
// operations.
type Operation interface {
	// Parent returns the parent operation, or nil for the root operation.
	Parent() Operation

	// unwrap is an internal method guaranteeing only *operation implements Operation.
	unwrap() *operation
}

// ArgOf marks a particular type as being the argument type of a given operation
// type. This allows this type to be listened to by an operation start listener.
// This removes the possibility of incorrectly pairing an operation and payload
// when setting up listeners, as it allows compiler-assisted coherence checks.
type ArgOf[O Operation] interface {
	IsArgOf(O)
}

// ResultOf marks a particular type as being the result type of a given
// operation. This allows this type to be listened to by an operation finish
// listener.
// This removes the possibility of incorrectly pairing an operation and payload
// when setting up listeners, as it allows compiler-assisted coherence checks.
type ResultOf[O Operation] interface {
	IsResultOf(O)
}

// EventListener interface allowing to identify the Go type listened to and
// dispatch calls to the underlying event listener function.
type EventListener[O Operation, T any] func(O, T)

// contextKey is used to store in a context.Context the ongoing Operation
type contextKey struct{}

// Atomic *Operation so we can atomically read or swap it.
var rootOperation atomic.Pointer[Operation]

// SwapRootOperation allows to atomically swap the current root operation with
// the given new one. Concurrent uses of the old root operation on already
// existing and running operation are still valid.
func SwapRootOperation(new Operation) {
	rootOperation.Swap(&new)
	// Note: calling Finish(old, ...) could result into mem leaks because
	// some finish event listeners, possibly releasing memory and resources,
	// wouldn't be called anymore (because Finish() disables the operation and
	// removes the event listeners).
}

// operation structure allowing to subscribe to operation events and to
// navigate in the operation stack. Events
// bubble-up the operation stack, which allows listening to future events that
// might happen in the operation lifetime.
type operation struct {
	parent Operation
	eventRegister
	dataBroadcaster

	disabled bool
	mu       sync.RWMutex

	// inContext is used to determine if RegisterOperation was called to put the Operation in the context tree.
	// If so we need to remove it from the context tree when the Operation is finished.
	inContext bool
}

func (o *operation) Parent() Operation {
	return o.parent
}

// This is the one true Operation implementation!
func (o *operation) unwrap() *operation { return o }

// NewRootOperation creates and returns a new root operation, with no parent
// operation. Root operations are meant to be the top-level operation of an
// operation stack, therefore receiving all the operation events. It allows to
// prepare a new set of event listeners, to then atomically swap it with the
// current one.
func NewRootOperation() Operation {
	return &operation{parent: nil}
}

// NewOperation creates and returns a new operation. It must be started by calling
// StartOperation, and finished by calling Finish. The returned operation should
// be used in wrapper types to provide statically typed start and finish
// functions. The following example shows how to wrap an operation so that its
// functions are statically typed (instead of dyngo's interface{} values):
//
//	package mypackage
//	import "dyngo"
//	type (
//	  MyOperation struct {
//	    dyngo.Operation
//	  }
//	  MyOperationArgs { /* ... */ }
//	  MyOperationRes { /* ... */ }
//	)
//	func StartOperation(args MyOperationArgs, parent dyngo.Operation) MyOperation {
//	  op := MyOperation{Operation: dyngo.NewOperation(parent)}
//	  dyngo.StartOperation(op, args)
//	  return op
//	}
//	func (op MyOperation) Finish(res MyOperationRes) {
//	    dyngo.FinishOperation(op, res)
//	  }
func NewOperation(parent Operation) Operation {
	if parent == nil {
		if ptr := rootOperation.Load(); ptr != nil {
			parent = *ptr
		}
	}

	return &operation{parent: parent}
}

// FromContext looks into the given context (or the GLS if orchestrion is enabled) for a parent Operation and returns it.
func FromContext(ctx context.Context) (Operation, bool) {
	ctx = orchestrion.WrapContext(ctx)
	if ctx == nil {
		return nil, false
	}

	op, ok := ctx.Value(contextKey{}).(Operation)
	return op, ok
}

// FindOperation looks into the current operation tree for the first operation matching the given type.
// It has a hardcoded limit of 32 levels of depth even looking for the operation in the parent tree
func FindOperation[T any, O interface {
	Operation
	*T
}](ctx context.Context) (*T, bool) {
	op, found := FromContext(ctx)
	if !found {
		return nil, false
	}

	for current := op; current != nil; current = current.unwrap().parent {
		if o, ok := current.(O); ok {
			return o, true
		}
	}

	return nil, false
}

// StartOperation starts a new operation along with its arguments and emits a
// start event with the operation arguments.
func StartOperation[O Operation, E ArgOf[O]](op O, args E) {
	// Bubble-up the start event starting from the parent operation as you can't
	// listen for your own start event
	for current := op.unwrap().parent; current != nil; current = current.unwrap().parent {
		emitEvent(&current.unwrap().eventRegister, op, args)
	}
}

// StartAndRegisterOperation calls StartOperation and returns RegisterOperation result
func StartAndRegisterOperation[O Operation, E ArgOf[O]](ctx context.Context, op O, args E) context.Context {
	StartOperation(op, args)
	return RegisterOperation(ctx, op)
}

// RegisterOperation registers the operation in the context tree. All operations that plan to have children operations
// should call this function to ensure the operation is properly linked in the context tree.
func RegisterOperation(ctx context.Context, op Operation) context.Context {
	op.unwrap().inContext = true
	return orchestrion.CtxWithValue(ctx, contextKey{}, op)
}

// FinishOperation finishes the operation along with its results and emits a
// finish event with the operation results.
// The operation is then disabled and its event listeners removed.
func FinishOperation[O Operation, E ResultOf[O]](op O, results E) {
	o := op.unwrap()
	defer o.disable() // This will need the RLock below to be released...

	o.mu.RLock()
	defer o.mu.RUnlock() // Deferred and stacked on top of the previously deferred call to o.disable()

	if o.inContext {
		orchestrion.GLSPopValue(contextKey{})
	}

	if o.disabled {
		return
	}

	var current Operation = op
	for ; current != nil; current = current.unwrap().parent {
		emitEvent(&current.unwrap().eventRegister, op, results)
	}
}

// Disable the operation and remove all its event listeners.
func (o *operation) disable() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.disabled {
		return
	}

	o.disabled = true
	o.eventRegister.clear()
}

// On registers and event listener that will be called when the operation
// begins.
func On[O Operation, E ArgOf[O]](op Operation, l EventListener[O, E]) {
	o := op.unwrap()

	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	addEventListener(&o.eventRegister, l)
}

// OnFinish registers an event listener that will be called when the operation
// finishes.
func OnFinish[O Operation, E ResultOf[O]](op Operation, l EventListener[O, E]) {
	o := op.unwrap()

	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	addEventListener(&o.eventRegister, l)
}

func OnData[T any](op Operation, l DataListener[T]) {
	o := op.unwrap()

	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	addDataListener(&o.dataBroadcaster, l)
}

// EmitData sends a data event up the operation stack. Listeners will be matched
// based on `T`. Callers may need to manually specify T when the static type of
// the value is more specific that the intended data event type.
func EmitData[T any](op Operation, data T) {
	o := op.unwrap()

	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	// Bubble up the data to the stack of operations. Contrary to events,
	// we also send the data to ourselves since SDK operations are leaf operations
	// that both emit and listen for data (errors).
	for current := op; current != nil; current = current.unwrap().parent {
		emitData(&current.unwrap().dataBroadcaster, data)
	}
}

type (
	// eventRegister implements a thread-safe list of event listeners.
	eventRegister struct {
		listeners eventListenerMap
		mu        sync.RWMutex
	}

	// eventListenerMap is the map of event listeners. The list of listeners are
	// indexed by the operation argument or result type the event listener
	// expects.
	eventListenerMap map[any][]any

	typeID[T any] struct{}

	dataBroadcaster struct {
		listeners dataListenerMap
		mu        sync.RWMutex
	}

	DataListener[T any] func(T)
	dataListenerMap     map[any][]any
)

func addDataListener[T any](b *dataBroadcaster, l DataListener[T]) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.listeners == nil {
		b.listeners = make(dataListenerMap)
	}
	key := typeID[DataListener[T]]{}
	b.listeners[key] = append(b.listeners[key], l)
}

func (b *dataBroadcaster) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listeners = nil
}

func emitData[T any](b *dataBroadcaster, v T) {
	defer func() {
		if r := recover(); r != nil {
			LogError("appsec: recovered from an unexpected panic from an event listener: %+v", r)
		}
	}()
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, listener := range b.listeners[typeID[DataListener[T]]{}] {
		listener.(DataListener[T])(v)
	}
}

func addEventListener[O Operation, T any](r *eventRegister, l EventListener[O, T]) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.listeners == nil {
		r.listeners = make(eventListenerMap, 2)
	}
	key := typeID[EventListener[O, T]]{}
	r.listeners[key] = append(r.listeners[key], l)
}

func (r *eventRegister) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = nil
}

func emitEvent[O Operation, T any](r *eventRegister, op O, v T) {
	defer func() {
		if r := recover(); r != nil {
			LogError("appsec: recovered from an unexpected panic from an event listener: %+v", r)
		}
	}()
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, listener := range r.listeners[typeID[EventListener[O, T]]{}] {
		listener.(EventListener[O, T])(op, v)
	}
}
