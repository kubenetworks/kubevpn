// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	wafErrors "github.com/DataDog/go-libddwaf/v3/errors"
	"github.com/DataDog/go-libddwaf/v3/internal/bindings"
	"github.com/DataDog/go-libddwaf/v3/internal/unsafe"
	"github.com/DataDog/go-libddwaf/v3/timer"
)

// Handle represents an instance of the WAF for a given ruleset.
type Handle struct {
	// diagnostics holds information about rules initialization
	diagnostics Diagnostics

	// Lock-less reference counter avoiding blocking calls to the Close() method
	// while WAF contexts are still using the WAF handle. Instead, we let the
	// release actually happen only when the reference counter reaches 0.
	// This can happen either from a request handler calling its WAF context's
	// Close() method, or either from the appsec instance calling the WAF
	// handle's Close() method when creating a new WAF handle with new rules.
	// Note that this means several instances of the WAF can exist at the same
	// time with their own set of rules. This choice was done to be able to
	// efficiently update the security rules concurrently, without having to
	// block the request handlers for the time of the security rules update.
	refCounter atomic.Int32

	// Instance of the WAF
	cHandle bindings.WafHandle
}

// NewHandle creates and returns a new instance of the WAF with the given security rules and configuration
// of the sensitive data obfuscator. The returned handle is nil in case of an error.
// Rules-related metrics, including errors, are accessible with the `RulesetInfo()` method.
func NewHandle(rules any, keyObfuscatorRegex string, valueObfuscatorRegex string) (*Handle, error) {
	// The order of action is the following:
	// - Open the ddwaf C library
	// - Encode the security rules as a ddwaf_object
	// - Create a ddwaf_config object and fill the values
	// - Run ddwaf_init to create a new handle based on the given rules and config
	// - Check for errors and streamline the ddwaf_ruleset_info returned

	if ok, err := Load(); !ok {
		return nil, err
		// The case where ok == true && err != nil is ignored on purpose, as
		// this is out of the scope of NewHandle which only requires a properly
		// loaded libddwaf in order to use it
	}

	encoder := newMaxEncoder()
	obj, err := encoder.Encode(rules)
	if err != nil {
		return nil, fmt.Errorf("could not encode the WAF ruleset into a WAF object: %w", err)
	}

	config := newConfig(&encoder.cgoRefs, keyObfuscatorRegex, valueObfuscatorRegex)
	diagnosticsWafObj := new(bindings.WafObject)
	defer wafLib.WafObjectFree(diagnosticsWafObj)

	cHandle := wafLib.WafInit(obj, config, diagnosticsWafObj)
	unsafe.KeepAlive(encoder.cgoRefs) // Keep this AFTER the call to wafLib.WafInit

	return newHandle(cHandle, diagnosticsWafObj)
}

// newHandle creates a new Handle from a C handle (nullable) and a diagnostics object.
// and it handles the multiple ways a WAF initialization can fail.
func newHandle(cHandle bindings.WafHandle, diagnosticsWafObj *bindings.WafObject) (*Handle, error) {
	diags, diagsErr := decodeDiagnostics(diagnosticsWafObj)
	if cHandle == 0 && diagsErr != nil { // WAF Failed initialization and we manage to decode the diagnostics, return the diagnostics error
		if err := diags.TopLevelError(); err != nil {
			return nil, fmt.Errorf("could not instantiate the WAF: %w", err)
		}
	}

	if cHandle == 0 {
		// WAF Failed initialization, report the best possible error...
		return nil, errors.New("could not instantiate the WAF")
	}

	// The WAF successfully initialized at this stage but if the diagnostics decoding failed, we still need to cleanup
	if diagsErr != nil {
		wafLib.WafDestroy(cHandle)
		return nil, fmt.Errorf("could not decode the WAF diagnostics: %w", diagsErr)
	}

	handle := &Handle{
		cHandle:     cHandle,
		diagnostics: diags,
	}

	handle.refCounter.Store(1) // We count the handle itself in the counter
	return handle, nil
}

// NewContext returns a new WAF context for the given WAF handle.
// A nil value is returned when the WAF handle was released or when the
// WAF context couldn't be created.
func (handle *Handle) NewContext() (*Context, error) {
	return handle.NewContextWithBudget(timer.UnlimitedBudget)
}

// NewContextWithBudget returns a new WAF context for the given WAF handle.
// A nil value is returned when the WAF handle was released or when the
// WAF context couldn't be created.
func (handle *Handle) NewContextWithBudget(budget time.Duration) (*Context, error) {
	// Handle has been released
	if !handle.retain() {
		return nil, fmt.Errorf("handle was released")
	}

	cContext := wafLib.WafContextInit(handle.cHandle)
	if cContext == 0 {
		handle.release() // We couldn't get a context, so we no longer have an implicit reference to the Handle in it...
		return nil, fmt.Errorf("could not get C context")
	}

	timer, err := timer.NewTreeTimer(timer.WithBudget(budget), timer.WithComponents(wafRunTag))
	if err != nil {
		return nil, err
	}

	return &Context{
		handle:      handle,
		cContext:    cContext,
		timer:       timer,
		metrics:     metricsStore{data: make(map[metricKey]time.Duration, 5)},
		truncations: make(map[Scope]map[TruncationReason][]int, 2),
		timeoutCount: map[Scope]*atomic.Uint64{
			DefaultScope: new(atomic.Uint64),
			RASPScope:    new(atomic.Uint64),
		},
	}, nil
}

// Diagnostics returns the rules initialization metrics for the current WAF handle
func (handle *Handle) Diagnostics() Diagnostics {
	return handle.diagnostics
}

// Addresses returns the list of addresses the WAF has been configured to monitor based on the input ruleset
func (handle *Handle) Addresses() []string {
	return wafLib.WafKnownAddresses(handle.cHandle)
}

// Actions returns the list of actions the WAF has been configured to monitor based on the input ruleset
func (handle *Handle) Actions() []string {
	return wafLib.WafKnownActions(handle.cHandle)
}

// Update the ruleset of a WAF instance into a new handle on its own
// the previous handle still needs to be closed manually
func (handle *Handle) Update(newRules any) (*Handle, error) {
	encoder := newMaxEncoder()
	obj, err := encoder.Encode(newRules)
	if err != nil {
		return nil, fmt.Errorf("could not encode the WAF ruleset into a WAF object: %w", err)
	}

	diagnosticsWafObj := new(bindings.WafObject)
	defer wafLib.WafObjectFree(diagnosticsWafObj)

	return newHandle(wafLib.WafUpdate(handle.cHandle, obj, diagnosticsWafObj), diagnosticsWafObj)
}

// Close puts the handle in termination state, when all the contexts are closed the handle will be destroyed
func (handle *Handle) Close() {
	if handle.addRefCounter(-1) != 0 {
		// Either the counter is still positive (this Handle is still referenced), or it had previously
		// reached 0 and some other call has done the cleanup already.
		return
	}

	wafLib.WafDestroy(handle.cHandle)
	handle.diagnostics = Diagnostics{} // Data in diagnostics may no longer be valid (e.g: strings from libddwaf)
	handle.cHandle = 0                 // Makes it easy to spot use-after-free/double-free issues
}

// retain increments the reference counter of this Handle. Returns true if the
// Handle is still valid, false if it is no longer usable. Calls to retain()
// must be balanced with calls to release() in order to avoid leaking Handles.
func (handle *Handle) retain() bool {
	return handle.addRefCounter(1) > 0
}

// release decrements the reference counter of this Handle, possibly causing it
// to be completely closed if no other reference to it exist.
func (handle *Handle) release() {
	handle.Close()
}

// addRefCounter adds x to Handle.refCounter. The return valid indicates whether the refCounter reached 0 as part of
// this call or not, which can be used to perform "only-once" activities:
// - result > 0    => the Handle is still usable
// - result == 0   => the handle is no longer usable, ref counter reached 0 as part of this call
// - result == -1  => the handle is no longer usable, ref counter was already 0 previously
func (handle *Handle) addRefCounter(x int32) int32 {
	// We use a CAS loop to avoid setting the refCounter to a negative value.
	for {
		current := handle.refCounter.Load()
		if current <= 0 {
			// The object had already been released
			return -1
		}

		next := current + x
		if swapped := handle.refCounter.CompareAndSwap(current, next); swapped {
			if next < 0 {
				// TODO(romain.marcadier): somehow signal unexpected behavior to the
				// caller (panic? error?). We currently clamp to 0 in order to avoid
				// causing a customer program crash, but this is the symptom of a bug
				// and should be investigated (however this clamping hides the issue).
				return 0
			}
			return next
		}
	}
}

func newConfig(cgoRefs *cgoRefPool, keyObfuscatorRegex string, valueObfuscatorRegex string) *bindings.WafConfig {
	config := new(bindings.WafConfig)
	*config = bindings.WafConfig{
		Limits: bindings.WafConfigLimits{
			MaxContainerDepth: bindings.WafMaxContainerDepth,
			MaxContainerSize:  bindings.WafMaxContainerSize,
			MaxStringLength:   bindings.WafMaxStringLength,
		},
		Obfuscator: bindings.WafConfigObfuscator{
			KeyRegex:   cgoRefs.AllocCString(keyObfuscatorRegex),
			ValueRegex: cgoRefs.AllocCString(valueObfuscatorRegex),
		},
		// Prevent libddwaf from freeing our Go-memory-allocated ddwaf_objects
		FreeFn: 0,
	}
	return config
}

func goRunError(rc bindings.WafReturnCode) error {
	switch rc {
	case bindings.WafErrInternal:
		return wafErrors.ErrInternal
	case bindings.WafErrInvalidObject:
		return wafErrors.ErrInvalidObject
	case bindings.WafErrInvalidArgument:
		return wafErrors.ErrInvalidArgument
	default:
		return fmt.Errorf("unknown waf return code %d", int(rc))
	}
}
