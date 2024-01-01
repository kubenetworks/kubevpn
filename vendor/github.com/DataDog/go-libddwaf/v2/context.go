// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"sync"
	"time"

	"go.uber.org/atomic"
)

// Context is a WAF execution context. It allows running the WAF incrementally
// when calling it multiple times to run its rules every time new addresses
// become available. Each request must have its own Context.
type Context struct {
	handle *Handle // Instance of the WAF

	cgoRefs  cgoRefPool // Used to retain go data referenced by WAF Objects the context holds
	cContext wafContext // The C ddwaf_context pointer

	// Stats
	totalRuntimeNs        atomic.Uint64 // Cumulative internal WAF run time - in nanoseconds - for this context.
	totalOverallRuntimeNs atomic.Uint64 // Cumulative overall run time - in nanoseconds - for this context.
	timeoutCount          atomic.Uint64 // Cumulative timeout count for this context.

	// Mutex protecting the use of cContext which is not thread-safe and cgoRefs.
	mutex sync.Mutex
}

// NewContext returns a new WAF context of to the given WAF handle.
// A nil value is returned when the WAF handle was released or when the
// WAF context couldn't be created.
// handle. A nil value is returned when the WAF handle can no longer be used
// or the WAF context couldn't be created.
func NewContext(handle *Handle) *Context {
	// Handle has been released
	if !handle.retain() {
		return nil
	}

	cContext := wafLib.wafContextInit(handle.cHandle)
	if cContext == 0 {
		handle.release() // We couldn't get a context, so we no longer have an implicit reference to the Handle in it...
		return nil
	}

	return &Context{handle: handle, cContext: cContext}
}

// RunAddressData provides address data to the Context.Run method. If a given key is present in both
// RunAddressData.Persistent and RunAddressData.Ephemeral, the value from RunAddressData.Persistent will take precedence.
type RunAddressData struct {
	// Persistent address data is scoped to the lifetime of a given Context, and subsquent calls to Context.Run with the
	// same address name will be silently ignored.
	Persistent map[string]any
	// Ephemeral address data is scoped to a given Context.Run call and is not persisted across calls. This is used for
	// protocols such as gRPC client/server streaming or GraphQL, where a single request can incur multiple subrequests.
	Ephemeral map[string]any
}

func (d RunAddressData) isEmpty() bool {
	return len(d.Persistent) == 0 && len(d.Ephemeral) == 0
}

// Run encodes the given addressData values and runs them against the WAF rules within the given timeout value. If a
// given address is present both as persistent and ephemeral, the persistent value takes precedence. It returns the
// matches as a JSON string (usually opaquely used) along with the corresponding actions in any. In case of an error,
// matches and actions can still be returned, for instance in the case of a timeout error. Errors can be tested against
// the RunError type.
func (context *Context) Run(addressData RunAddressData, timeout time.Duration) (res Result, err error) {
	if addressData.isEmpty() {
		return
	}

	now := time.Now()
	defer func() {
		dt := time.Since(now)
		context.totalOverallRuntimeNs.Add(uint64(dt.Nanoseconds()))
	}()

	// At this point, the only error we can get is an error in case the top level object is a nil map, but this
	// behaviour is expected since either persistent or ephemeral addresses are allowed to be null one at a time.
	// In this case, EncodeAddresses will return nil contrary to Encode which will return an nil wafObject,
	// which is what we need to send to ddwaf_run to signal that the address data is empty.
	var persistentData *wafObject = nil
	var ephemeralData *wafObject = nil
	persistentEncoder := newLimitedEncoder()
	ephemeralEncoder := newLimitedEncoder()
	if addressData.Persistent != nil {
		persistentData, _ = persistentEncoder.EncodeAddresses(addressData.Persistent)
	}

	if addressData.Ephemeral != nil {
		ephemeralData, _ = ephemeralEncoder.EncodeAddresses(addressData.Ephemeral)

	}
	// The WAF releases ephemeral address data at the end of each run call, so we need not keep the Go values live beyond
	// that in the same way we need for persistent data. We hence use a separate encoder.

	// ddwaf_run cannot run concurrently and the next append write on the context state so we need a mutex
	context.mutex.Lock()
	defer context.mutex.Unlock()

	// Save the Go pointer references to addressesToData that were referenced by the encoder
	// into C ddwaf_objects. libddwaf's API requires to keep this data for the lifetime of the ddwaf_context.
	defer context.cgoRefs.append(persistentEncoder.cgoRefs)

	res, err = context.run(persistentData, ephemeralData, timeout, &persistentEncoder.cgoRefs)

	// Ensure the ephemerals don't get optimized away by the compiler before the WAF had a chance to use them.
	keepAlive(ephemeralEncoder.cgoRefs)

	return
}

// run executes the ddwaf_run call with the provided data on this context. The caller is responsible for locking the
// context appropriately around this call.
func (context *Context) run(persistentData, ephemeralData *wafObject, timeout time.Duration, cgoRefs *cgoRefPool) (Result, error) {
	result := new(wafResult)
	defer wafLib.wafResultFree(result)

	ret := wafLib.wafRun(context.cContext, persistentData, ephemeralData, result, uint64(timeout/time.Microsecond))

	context.totalRuntimeNs.Add(result.total_runtime)
	res, err := unwrapWafResult(ret, result)
	if err == ErrTimeout {
		context.timeoutCount.Inc()
	}

	return res, err
}

func unwrapWafResult(ret wafReturnCode, result *wafResult) (res Result, err error) {
	if result.timeout > 0 {
		err = ErrTimeout
	} else {
		// Derivatives can be generated even if no security event gets detected, so we decode them as long as the WAF
		// didn't timeout
		res.Derivatives, err = decodeMap(&result.derivatives)
	}

	if ret == wafOK {
		return res, err
	}

	if ret != wafMatch {
		return res, goRunError(ret)
	}

	res.Events, err = decodeArray(&result.events)
	if err != nil {
		return res, err
	}
	if size := result.actions.nbEntries; size > 0 {
		// using ruleIdArray cause it decodes string array (I think)
		res.Actions, err = decodeStringArray(&result.actions)
		// TODO: use decode array, and eventually genericize the function
		if err != nil {
			return res, err
		}
	}

	return res, err
}

// Close the underlying `ddwaf_context` and releases the associated internal
// data. Also decreases the reference count of the `ddwaf_hadnle` which created
// this context, possibly releasing it completely (if this was the last context
// created from this handle & it was released by its creator).
func (context *Context) Close() {
	context.mutex.Lock()
	defer context.mutex.Unlock()

	wafLib.wafContextDestroy(context.cContext)
	keepAlive(context.cgoRefs)     // Keep the Go pointer references until the end of the context
	defer context.handle.release() // Reduce the reference counter of the Handle.

	context.cgoRefs = cgoRefPool{} // The data in context.cgoRefs is no longer needed, explicitly release
	context.cContext = 0           // Makes it easy to spot use-after-free/double-free issues
}

// TotalRuntime returns the cumulated WAF runtime across various run calls within the same WAF context.
// Returned time is in nanoseconds.
func (context *Context) TotalRuntime() (overallRuntimeNs, internalRuntimeNs uint64) {
	return context.totalOverallRuntimeNs.Load(), context.totalRuntimeNs.Load()
}

// TotalTimeouts returns the cumulated amount of WAF timeouts across various run calls within the same WAF context.
func (context *Context) TotalTimeouts() uint64 {
	return context.timeoutCount.Load()
}
