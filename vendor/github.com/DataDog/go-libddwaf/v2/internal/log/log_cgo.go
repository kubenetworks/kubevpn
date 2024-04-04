// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build cgo && !datadog.no_waf

package log

// #include "./ddwaf.h"
// extern void ddwafLogCallbackFn(
//   DDWAF_LOG_LEVEL level,
//   char* function,
//   char* file,
//   unsigned line,
//   char* message,
//   uint64_t message_len
// );
import "C"
import "github.com/DataDog/go-libddwaf/v2/internal/unsafe"

// CallbackFunctionPointer returns a pointer to the log callback function which
// can be used with libddwaf.
func CallbackFunctionPointer() uintptr {
	return uintptr(C.ddwafLogCallbackFn)
}

//export ddwafLogCallbackFn
func ddwafLogCallbackFn(level C.DDWAF_LOG_LEVEL, fnPtr, filePtr *C.char, line C.unsigned, msgPtr *C.char, _ C.uint64_t) {
	function := unsafe.Gostring(unsafe.CastNative[C.char, byte](fnPtr))
	file := unsafe.Gostring(unsafe.CastNative[C.char, byte](filePtr))
	message := unsafe.Gostring(unsafe.CastNative[C.char, byte](msgPtr))

	logMessage(Level(level), function, file, uint(line), message)
}
