// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build !cgo && (darwin || freebsd) && !datadog.no_waf && !go1.23

package log

import (
	"github.com/DataDog/go-libddwaf/v2/internal/unsafe"
	"sync"

	"github.com/ebitengine/purego"
)

var (
	once            sync.Once
	functionPointer uintptr
)

// CallbackFunctionPointer returns a pointer to the log callback function which
// can be used with libddwaf.
func CallbackFunctionPointer() uintptr {
	once.Do(func() {
		functionPointer = purego.NewCallback(ddwafLogCallbackFn)
	})
	return functionPointer
}

func ddwafLogCallbackFn(level Level, fnPtr, filePtr *byte, line uint, msgPtr *byte, _ uint64) {
	function := unsafe.Gostring(fnPtr)
	file := unsafe.Gostring(filePtr)
	message := unsafe.Gostring(msgPtr)

	logMessage(level, function, file, line, message)
}
