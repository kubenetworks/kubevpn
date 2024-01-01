// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package noopfree provides a noop-ed free function. A separate package is
// needed to avoid the special go-build case with CGO enabled where it compiles
// .s files with CC instead of the Go assembler that we want here.
package noopfree

import "unsafe"

//go:linkname _noop_free_v2 _noop_free_v2
var _noop_free_v2 byte
var NoopFreeFn uintptr = uintptr(unsafe.Pointer(&_noop_free_v2))
