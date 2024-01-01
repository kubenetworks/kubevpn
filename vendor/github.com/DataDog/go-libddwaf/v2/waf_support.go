// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"fmt"
	"runtime"

	"github.com/hashicorp/go-multierror"
)

// Errors used to report data using the Health function
// Store all the errors related to why go-liddwaf is unavailable for the current target at runtime.
var wafSupportErrors []error

// Not nil if the build tag `datadog.no_waf` is set
var wafManuallyDisabledErr error

// UnsupportedOSArchError is a wrapper error type helping to handle the error
// case of trying to execute this package when the OS or architecture is not supported.
type UnsupportedOSArchError struct {
	Os   string
	Arch string
}

func (e UnsupportedOSArchError) Error() string {
	return fmt.Sprintf("unsupported OS/Arch: %s/%s", e.Os, e.Arch)
}

// UnsupportedGoVersionError is a wrapper error type helping to handle the error
// case of trying to execute this package when the Go version is not supported.
type UnsupportedGoVersionError struct{}

func (e UnsupportedGoVersionError) Error() string {
	return fmt.Sprintf("unsupported Go version: %s", runtime.Version())
}

type CgoDisabledError struct{}

func (e CgoDisabledError) Error() string {
	return "go-libddwaf is disabled when cgo is disabled unless you compile with the go build tag `appsec`. It will require libdl.so.2. libpthread.so.0, libc.so.6 and libm.so.6 shared libraries at run time on linux"
}

// ManuallyDisabledError is a wrapper error type helping to handle the error
// case of trying to execute this package when the WAF has been manually disabled with
// the `datadog.no_waf` go build tag.
type ManuallyDisabledError struct{}

func (e ManuallyDisabledError) Error() string {
	return "the WAF has been manually disabled using the `datadog.no_waf` go build tag"
}

// SupportsTarget returns true and a nil error when the target host environment
// is supported by this package and can be further used.
// Otherwise, it returns false along with an error detailing why.
func SupportsTarget() (bool, error) {
	return len(wafSupportErrors) == 0, multierror.Append(nil, wafSupportErrors...).ErrorOrNil()
}

// Health returns true if the waf is usable, false otherwise. At the same time it can return an error
// if the waf is not usable, but the error is not blocking if true is returned, otherwise it is.
// The following conditions are checked:
// - The Waf library has been loaded successfully (you need to call `Load()` first for this case to be taken into account)
// - The Waf library has not been manually disabled with the `datadog.no_waf` go build tag
// - The Waf library is not in an unsupported OS/Arch
// - The Waf library is not in an unsupported Go version
func Health() (bool, error) {
	var err *multierror.Error
	if wafLoadErr != nil {
		err = multierror.Append(err, wafLoadErr)
	}

	if len(wafSupportErrors) > 0 {
		err = multierror.Append(err, wafSupportErrors...)
	}

	if wafManuallyDisabledErr != nil {
		err = multierror.Append(err, wafManuallyDisabledErr)
	}

	return (wafLib != nil || wafLoadErr == nil) && len(wafSupportErrors) == 0 && wafManuallyDisabledErr == nil, err.ErrorOrNil()
}
