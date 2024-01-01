// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"errors"
	"fmt"
	"sync"

	"github.com/hashicorp/go-multierror"
)

// Diagnostics stores the information - provided by the WAF - about WAF rules initialization.
type Diagnostics struct {
	Rules          *DiagnosticEntry
	CustomRules    *DiagnosticEntry
	Exclusions     *DiagnosticEntry
	RulesOverrides *DiagnosticEntry
	RulesData      *DiagnosticEntry
	Processors     *DiagnosticEntry
	Scanners       *DiagnosticEntry
	Version        string
}

// TopLevelErrors returns the list of top-level errors reported by the WAF on any of the Diagnostics
// entries, rolled up into a single error value. Returns nil if no top-level errors were reported.
// Individual, item-level errors might still exist.
func (d *Diagnostics) TopLevelError() error {
	fields := map[string]*DiagnosticEntry{
		"rules":          d.Rules,
		"custom_rules":   d.CustomRules,
		"exclusions":     d.Exclusions,
		"rules_override": d.RulesOverrides,
		"rules_data":     d.RulesData,
		"processors":     d.Processors,
		"scanners":       d.Scanners,
	}

	var err *multierror.Error
	for field, entry := range fields {
		if entry == nil || entry.Error == "" {
			// No entry or no error => we're all good.
			continue
		}
		// TODO: rely on errors.Join() once go1.20 is our min supported Go version
		err = multierror.Append(err, fmt.Errorf("in %#v: %s", field, entry.Error))
	}

	return err.ErrorOrNil()
}

// DiagnosticEntry stores the information - provided by the WAF - about loaded and failed rules
// for a specific entry in the WAF ruleset
type DiagnosticEntry struct {
	Addresses *DiagnosticAddresses
	Errors    map[string][]string // Item-level errors (map of error message to entity identifiers or index:#)
	Error     string              // If the entire entry was in error (e.g: invalid format)
	Loaded    []string            // Successfully loaded entity identifiers (or index:#)
	Failed    []string            // Failed entity identifiers (or index:#)
}

// DiagnosticAddresses stores the information - provided by the WAF - about the known addresses and
// whether they are required or optional. Addresses used by WAF rules are always required. Addresses
// used by WAF exclusion filters may be required or (rarely) optional. Addresses used by WAF
// processors may be required or optional.
type DiagnosticAddresses struct {
	Required []string
	Optional []string
}

// Result stores the multiple values returned by a call to ddwaf_run
type Result struct {
	Events      []any
	Derivatives map[string]any
	Actions     []string
}

// Encoder/Decoder errors
var (
	errMaxDepthExceeded  = errors.New("max depth exceeded")
	errUnsupportedValue  = errors.New("unsupported Go value")
	errInvalidMapKey     = errors.New("invalid WAF object map key")
	errNilObjectPtr      = errors.New("nil WAF object pointer")
	errInvalidObjectType = errors.New("invalid type encountered when decoding")
)

// RunError the WAF can return when running it.
type RunError int

// Errors the WAF can return when running it.
const (
	ErrInternal RunError = iota + 1
	ErrInvalidObject
	ErrInvalidArgument
	ErrTimeout
	ErrOutOfMemory
	ErrEmptyRuleAddresses
)

// Error returns the string representation of the RunError.
func (e RunError) Error() string {
	switch e {
	case ErrInternal:
		return "internal waf error"
	case ErrTimeout:
		return "waf timeout"
	case ErrInvalidObject:
		return "invalid waf object"
	case ErrInvalidArgument:
		return "invalid waf argument"
	case ErrOutOfMemory:
		return "out of memory"
	case ErrEmptyRuleAddresses:
		return "empty rule addresses"
	default:
		return fmt.Sprintf("unknown waf error %d", e)
	}
}

// Globally dlopen() libddwaf only once because several dlopens (eg. in tests)
// aren't supported by macOS.
var (
	// libddwaf's dynamic library handle and entrypoints
	wafLib *wafDl
	// libddwaf's dlopen error if any
	wafLoadErr  error
	openWafOnce sync.Once
)

// Load loads libddwaf's dynamic library. The dynamic library is opened only
// once by the first call to this function and internally stored globally, and
// no function is currently provided in this API to close the opened handle.
// Calling this function is not mandatory and is automatically performed by
// calls to NewHandle, the entrypoint of libddwaf, but Load is useful in order
// to explicitly check libddwaf's general health where calling NewHandle doesn't
// necessarily apply nor is doable.
// The function returns ok when libddwaf was successfully loaded, along with a
// non-nil error if any. Note that both ok and err can be set, meaning that
// libddwaf is usable but some non-critical errors happened, such as failures
// to remove temporary files. It is safe to continue using libddwaf in such
// case.
func Load() (ok bool, err error) {
	if ok, err = Health(); !ok {
		return false, err
	}

	openWafOnce.Do(func() {
		wafLib, wafLoadErr = newWafDl()
		if wafLoadErr != nil {
			return
		}
		wafVersion = wafLib.wafGetVersion()
	})

	return wafLib != nil, wafLoadErr
}

var wafVersion string

// Version returns the version returned by libddwaf.
// It relies on the dynamic loading of the library, which can fail and return
// an empty string or the previously loaded version, if any.
func Version() string {
	Load()
	return wafVersion
}

// HasEvents return true if the result holds at least 1 event
func (r *Result) HasEvents() bool {
	return len(r.Events) > 0
}

// HasDerivatives return true if the result holds at least 1 derivative
func (r *Result) HasDerivatives() bool {
	return len(r.Derivatives) > 0
}

// HasActions return true if the result holds at least 1 action
func (r *Result) HasActions() bool {
	return len(r.Actions) > 0
}
