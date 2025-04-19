// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package errors

import (
	"errors"
	"fmt"
)

// Encoder/Decoder errors
var (
	ErrMaxDepthExceeded    = errors.New("max depth exceeded")
	ErrUnsupportedValue    = errors.New("unsupported Go value")
	ErrInvalidMapKey       = errors.New("invalid WAF object map key")
	ErrNilObjectPtr        = errors.New("nil WAF object pointer")
	ErrInvalidObjectType   = errors.New("invalid type encountered when decoding")
	ErrTooManyIndirections = errors.New("too many indirections")
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

var errorStrMap = map[RunError]string{
	ErrInternal:           "internal waf error",
	ErrInvalidObject:      "invalid waf object",
	ErrInvalidArgument:    "invalid waf argument",
	ErrTimeout:            "waf timeout",
	ErrOutOfMemory:        "out of memory",
	ErrEmptyRuleAddresses: "empty rule addresses",
}

// Error returns the string representation of the RunError.
func (e RunError) Error() string {
	description, ok := errorStrMap[e]
	if !ok {
		return fmt.Sprintf("unknown waf error %d", e)
	}

	return description
}

// PanicError is an error type wrapping a recovered panic value that happened
// during a function call. Such error must be considered unrecoverable and be
// used to try to gracefully abort. Keeping using this package after such an
// error is unreliable and the caller must rather stop using the library.
// Examples include safety checks errors.
type PanicError struct {
	// The recovered panic error while executing the function `in`.
	Err error
	// The function symbol name that was given to `tryCall()`.
	In string
}

// Unwrap the error and return it.
// Required by errors.Is and errors.As functions.
func (e *PanicError) Unwrap() error {
	return e.Err
}

// Error returns the error string representation.
func (e *PanicError) Error() string {
	return fmt.Sprintf("panic while executing %s: %#+v", e.In, e.Err)
}
