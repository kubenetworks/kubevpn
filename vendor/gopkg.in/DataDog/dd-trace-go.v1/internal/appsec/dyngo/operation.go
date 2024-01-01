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
	"reflect"
	"sync"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	"go.uber.org/atomic"
)

// Operation interface type allowing to register event listeners to the
// operation. The event listeners will be automatically removed from the
// operation once it finishes so that it no longer can be called on finished
// operations.
type Operation interface {
	// On allows to register an event listener to the operation. The event
	// listener will be removed from the operation once it finishes.
	On(EventListener)

	// OnData allows to register a data listener to the operation
	OnData(DataListener)

	// EmitData sends data to the data listeners of the operation
	EmitData(any)

	// Parent return the parent operation. It returns nil for the root
	// operation.
	Parent() Operation

	// emitEvent emits the event to listeners of the given argsType and calls
	// them with the given op and v values.
	// emitEvent is a private method implemented by the operation struct type so
	// that no other package can define it.
	emitEvent(argsType reflect.Type, op Operation, v interface{})

	emitData(argsType reflect.Type, v any)

	// add the given event listeners to the operation.
	// add is a private method implemented by the operation struct type so
	// that no other package can define it.
	add(...EventListener)

	// finish the operation. This method allows to pass the operation value to
	// use to emit the finish event.
	// finish is a private method implemented by the operation struct type so
	// that no other package can define it.
	finish(op Operation, results interface{})
}

// EventListener interface allowing to identify the Go type listened to and
// dispatch calls to the underlying event listener function.
type EventListener interface {
	// ListenedType returns the Go type the event listener listens to.
	ListenedType() reflect.Type
	// Call the underlying event listener function. The type of the value v
	// is the type the event listener listens to, according to the type
	// returned by ListenedType().
	Call(op Operation, v interface{})
}

// Atomic *Operation so we can atomically read or swap it.
var rootOperation atomic.Pointer[Operation]

// SwapRootOperation allows to atomically swap the current root operation with
// the given new one. Concurrent uses of the old root operation on already
// existing and running operation are still valid.
func SwapRootOperation(new Operation) {
	rootOperation.Swap(&new)
	// Note: calling FinishOperation(old) could result into mem leaks because
	// some finish event listeners, possibly releasing memory and resources,
	// wouldn't be called anymore (because finish() disables the operation and
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
}

// NewRootOperation creates and returns a new root operation, with no parent
// operation. Root operations are meant to be the top-level operation of an
// operation stack, therefore receiving all the operation events. It allows to
// prepare a new set of event listeners, to then atomically swap it with the
// current one.
func NewRootOperation() Operation {
	return newOperation(nil)
}

// NewOperation creates and returns a new operation. It must be started by calling
// StartOperation, and finished by calling FinishOperation. The returned
// operation should be used in wrapper types to provide statically typed start
// and finish functions. The following example shows how to wrap an operation
// so that its functions are statically typed (instead of dyngo's interface{}
// values):
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
		if root := rootOperation.Load(); root != nil {
			parent = *root
		}
	}
	return newOperation(parent)
}

// StartOperation starts a new operation along with its arguments and emits a
// start event with the operation arguments.
func StartOperation(op Operation, args interface{}) {
	argsType := reflect.TypeOf(args)
	// Bubble-up the start event starting from the parent operation as you can't
	// listen for your own start event
	for current := op.Parent(); current != nil; current = current.Parent() {
		current.emitEvent(argsType, op, args)
	}
}

func newOperation(parent Operation) *operation {
	return &operation{parent: parent}
}

// Parent return the parent operation. It returns nil for the root operation.
func (o *operation) Parent() Operation {
	return o.parent
}

// FinishOperation finishes the operation along with its results and emits a
// finish event with the operation results.
// The operation is then disabled and its event listeners removed.
func FinishOperation(op Operation, results interface{}) {
	op.finish(op, results)
}

func (o *operation) finish(op Operation, results interface{}) {
	// Defer the call to o.disable() first so that the RWMutex gets unlocked first
	defer o.disable()
	o.mu.RLock()
	defer o.mu.RUnlock() // Deferred and stacked on top of the previously deferred call to o.disable()
	if o.disabled {
		return
	}
	resType := reflect.TypeOf(results)
	for current := op; current != nil; current = current.Parent() {
		current.emitEvent(resType, op, results)
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

// Add the given event listeners to the operation.
func (o *operation) add(l ...EventListener) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	for _, l := range l {
		if l == nil {
			continue
		}
		key := l.ListenedType()
		o.eventRegister.add(key, l)
	}
}

// On registers the event listener. The difference with the Register() is that
// it doesn't return a function closure, which avoids unnecessary allocations
// For example:
//
//	op.On(MyOperationStart(func (op MyOperation, args MyOperationArgs) {
//	    // ...
//	}))
func (o *operation) On(l EventListener) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	o.eventRegister.add(l.ListenedType(), l)
}

func (o *operation) OnData(l DataListener) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	o.dataBroadcaster.add(l.ListenedType(), l)
}

func (o *operation) EmitData(data any) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.disabled {
		return
	}
	// Bubble up the data to the stack of operations. Contrary to events,
	// we also send the data to ourselves since SDK operations are leaf operations
	// that both emit and listen for data (errors).
	for current := Operation(o); current != nil; current = current.Parent() {
		current.emitData(reflect.TypeOf(data), data)
	}
}

type (
	// eventRegister implements a thread-safe list of event listeners.
	eventRegister struct {
		mu        sync.RWMutex
		listeners eventListenerMap
	}

	// eventListenerMap is the map of event listeners. The list of listeners are
	// indexed by the operation argument or result type the event listener
	// expects.
	eventListenerMap map[reflect.Type][]EventListener

	dataBroadcaster struct {
		mu        sync.RWMutex
		listeners dataListenerMap
	}

	dataListenerSpec[T any] func(data T)
	DataListener            EventListener
	dataListenerMap         map[reflect.Type][]DataListener
)

func (l dataListenerSpec[T]) Call(_ Operation, v interface{}) {
	l(v.(T))
}

func (l dataListenerSpec[T]) ListenedType() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// NewDataListener creates a specialized generic data listener, wrapped under a DataListener interface
func NewDataListener[T any](f func(data T)) DataListener {
	return dataListenerSpec[T](f)
}

func (b *dataBroadcaster) add(key reflect.Type, l DataListener) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.listeners == nil {
		b.listeners = make(dataListenerMap)
	}
	b.listeners[key] = append(b.listeners[key], l)

}

func (b *dataBroadcaster) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listeners = nil
}

func (b *dataBroadcaster) emitData(key reflect.Type, v any) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("appsec: recovered from an unexpected panic from an event listener: %+v", r)
		}
	}()
	b.mu.RLock()
	defer b.mu.RUnlock()
	for t := range b.listeners {
		if key == t || key.Implements(t) {
			for _, listener := range b.listeners[t] {
				listener.Call(nil, v)
			}
		}
	}
}

func (r *eventRegister) add(key reflect.Type, l EventListener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listeners == nil {
		r.listeners = make(eventListenerMap)
	}
	r.listeners[key] = append(r.listeners[key], l)
}

func (r *eventRegister) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = nil
}

func (r *eventRegister) emitEvent(key reflect.Type, op Operation, v interface{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("appsec: recovered from an unexpected panic from an event listener: %+v", r)
		}
	}()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, listener := range r.listeners[key] {
		listener.Call(op, v)
	}
}
