// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !cgo && appsec && linux && !go1.22 && !datadog.no_waf

package waf

// Adds a dynamic import for libm.so because libddwaf needs the ceilf symbol
// This mechanic only works when CGO is not enabled
//
//go:cgo_import_dynamic purego_ceilf ceilf "libm.so.6"
//go:cgo_import_dynamic _ _ "libm.so.6"
var purego_ceilf uintptr
