// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package unsafe

import (
	"reflect"
	stdUnsafe "unsafe"
)

// Gostring copies a char* to a Go string.
func Gostring(ptr *byte) string {
	if ptr == nil {
		return ""
	}
	var length int
	for {
		if *(*byte)(stdUnsafe.Add(stdUnsafe.Pointer(ptr), uintptr(length))) == '\x00' {
			break
		}
		length++
	}
	//string builtin copies the slice
	return string(stdUnsafe.Slice(ptr, length))
}

// NativeStringUnwrap cast a native string type into it's runtime value. Exported as the struct reflect.StringHeader
func NativeStringUnwrap(str string) reflect.StringHeader {
	return *(*reflect.StringHeader)(stdUnsafe.Pointer(&str))
}

func GostringSized(ptr *byte, size uint64) string {
	if ptr == nil {
		return ""
	}
	return string(stdUnsafe.Slice(ptr, size))
}

// Cstring converts a go string to *byte that can be passed to C code.
func Cstring(name string) *byte {
	var b = make([]byte, len(name)+1)
	copy(b, name)
	return &b[0]
}

// Cast is used to centralize unsafe use C of allocated pointer.
// We take the address and then dereference it to trick go vet from creating a possible misuse of unsafe.Pointer
func Cast[T any](ptr uintptr) *T {
	return (*T)(*(*stdUnsafe.Pointer)(stdUnsafe.Pointer(&ptr)))
}

type Native interface {
	~byte | ~float64 | ~float32 | ~int | ~int8 | ~int16 | ~int32 | ~int64 | ~bool | ~uintptr
}

func CastNative[N Native, T Native](ptr *N) *T {
	return (*T)(*(*stdUnsafe.Pointer)(stdUnsafe.Pointer(&ptr)))
}

// NativeToUintptr is a helper used by populate WafObject values
// with Go values
func NativeToUintptr[T any](x T) uintptr {
	return *(*uintptr)(stdUnsafe.Pointer(&x))
}

// UintToNative is a helper used retrieve Go values from an uintptr encoded
// value from a WafObject
func UintptrToNative[T any](x uintptr) T {
	return *(*T)(stdUnsafe.Pointer(&x))
}

// CastWithOffset is the same as cast but adding an offset to the pointer by a multiple of the size
// of the type pointed.
func CastWithOffset[T any](ptr uintptr, offset uint64) *T {
	return (*T)(stdUnsafe.Add(*(*stdUnsafe.Pointer)(stdUnsafe.Pointer(&ptr)), offset*uint64(stdUnsafe.Sizeof(*new(T)))))
}

// PtrToUintptr is a helper to centralize of usage of unsafe.Pointer
// do not use this function to cast interfaces
func PtrToUintptr[T any](arg *T) uintptr {
	return uintptr(stdUnsafe.Pointer(arg))
}

func SliceToUintptr[T any](arg []T) uintptr {
	return (*reflect.SliceHeader)(stdUnsafe.Pointer(&arg)).Data
}

// KeepAlive() globals
var (
	alwaysFalse bool
	escapeSink  any
)

// KeepAlive is a copy of runtime.KeepAlive
// keepAlive has 2 usages:
// - It forces the deallocation of the memory to take place later than expected (just like runtime.KeepAlive)
// - It forces the given argument x to be escaped on the heap by saving it into a global value (Go doesn't provide a standard way to do it as of today)
// It is implemented so that the compiler cannot optimize it.
//
//go:noinline
func KeepAlive[T any](x T) {
	if alwaysFalse {
		escapeSink = x
	}
}
