// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"fmt"
	"reflect"
	"runtime"

	"github.com/pkg/errors"
)

// PanicError is an error type wrapping a recovered panic value that happened
// during a function call. Such error must be considered unrecoverable and be
// used to try to gracefully abort. Keeping using this package after such an
// error is unreliable and the caller must rather stop using the library.
// Examples include safety checks errors.
type PanicError struct {
	// The recovered panic error while executing the function `in`.
	Err error
	// The function symbol name that was given to `tryCall()`.
	in string
}

func newPanicError(in func() error, err error) *PanicError {
	return &PanicError{
		in:  runtime.FuncForPC(reflect.ValueOf(in).Pointer()).Name(),
		Err: err,
	}
}

// Unwrap the error and return it.
// Required by errors.Is and errors.As functions.
func (e *PanicError) Unwrap() error {
	return e.Err
}

// Error returns the error string representation.
func (e *PanicError) Error() string {
	return fmt.Sprintf("panic while executing %s: %#+v", e.in, e.Err)
}

// tryCall calls function `f` and recovers from any panic occurring while it
// executes, returning it in a `PanicError` object type.
func tryCall(f func() error) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			// Note that panic(nil) matches this case and cannot be really tested for.
			return
		}

		switch actual := r.(type) {
		case error:
			err = errors.WithStack(actual)
		case string:
			err = errors.New(actual)
		default:
			err = errors.Errorf("%v", r)
		}

		err = newPanicError(f, err)
	}()
	return f()
}
