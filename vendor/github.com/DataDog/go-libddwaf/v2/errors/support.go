// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package errors

import (
	"fmt"
	"runtime"
)

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
	return "go-libddwaf is disabled when cgo is disabled unless you compile with the go build tag `appsec`. It will require libdl.so.2. libpthread.so.0 and libc.so.6 shared libraries at run time on linux"
}

// ManuallyDisabledError is a wrapper error type helping to handle the error
// case of trying to execute this package when the WAF has been manually disabled with
// the `datadog.no_waf` go build tag.
type ManuallyDisabledError struct{}

func (e ManuallyDisabledError) Error() string {
	return "the WAF has been manually disabled using the `datadog.no_waf` go build tag"
}
