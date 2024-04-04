// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build (!cgo && ((!darwin && !freebsd) || go1.23)) || datadog.no_waf

package log

// CallbackFunctionPointer returns a NULL pointer since this particular platform
// configuration is not supported by purego, and cgo is disabled.
func CallbackFunctionPointer() uintptr {
	return 0
}
