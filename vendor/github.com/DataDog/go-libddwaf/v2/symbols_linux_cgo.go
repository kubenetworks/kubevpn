// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build cgo && linux && !go1.22 && !datadog.no_waf

package waf

/*
// Needed otherwise cielf call is optimized away or the builtin version is used
#cgo CFLAGS: -O0
#cgo LDFLAGS: -lm
float __attribute__((__noinline__)) ceilf(float arg);
*/
import "C"

// Required because libddwaf uses ceilf located in libm
// This forces CGO to link with libm, from there since
// libm is loaded, we can dlopen the waf without issues
var _ = C.ceilf(2.3)
