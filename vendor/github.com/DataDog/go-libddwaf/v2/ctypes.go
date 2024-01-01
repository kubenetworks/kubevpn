// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"reflect"
	"unsafe"
)

const (
	wafMaxStringLength   = 4096
	wafMaxContainerDepth = 20
	wafMaxContainerSize  = 256
	wafRunTimeout        = 5000
)

type wafReturnCode int32

const (
	wafErrInternal        wafReturnCode = -3
	wafErrInvalidObject                 = -2
	wafErrInvalidArgument               = -1
	wafOK                               = 0
	wafMatch                            = 1
)

// wafObjectType is an enum in C which has the size of DWORD.
// But DWORD is 4 bytes in amd64 and arm64 so uint32 it is.
type wafObjectType uint32

const (
	wafInvalidType wafObjectType = 0
	wafIntType                   = 1 << 0
	wafUintType                  = 1 << 1
	wafStringType                = 1 << 2
	wafArrayType                 = 1 << 3
	wafMapType                   = 1 << 4
	wafBoolType                  = 1 << 5
	wafFloatType                 = 1 << 6
	wafNilType                   = 1 << 7
)

type wafObject struct {
	parameterName       uintptr
	parameterNameLength uint64
	value               uintptr
	nbEntries           uint64
	_type               wafObjectType
	_                   [4]byte
	// Forced padding
	// We only support 2 archs and cgo generated the same padding to both.
	// We don't want the C struct to be packed because actually go will do the same padding itself,
	// we just add it explicitly to not take any chance.
	// And we cannot pack a struct in go so it will get tricky if the struct is
	// packed (apart from breaking all tracers of course)
}

// isInvalid determines whether this WAF Object has the invalid type (which is the 0-value).
func (w *wafObject) isInvalid() bool {
	return w._type == wafInvalidType
}

// isNil determines whether this WAF Object is nil or not.
func (w *wafObject) isNil() bool {
	return w._type == wafNilType
}

// isArray determines whether this WAF Object is an array or not.
func (w *wafObject) isArray() bool {
	return w._type == wafArrayType
}

// isMap determines whether this WAF Object is a map or not.
func (w *wafObject) isMap() bool {
	return w._type == wafMapType
}

// IsUnusable returns true if the wafObject has no impact on the WAF execution
// But we still need this kind of objects to forward map keys in case the value of the map is invalid
func (wo *wafObject) IsUnusable() bool {
	return wo._type == wafInvalidType || wo._type == wafNilType
}

type wafConfig struct {
	limits     wafConfigLimits
	obfuscator wafConfigObfuscator
	freeFn     uintptr
}

type wafConfigLimits struct {
	maxContainerSize  uint32
	maxContainerDepth uint32
	maxStringLength   uint32
}

type wafConfigObfuscator struct {
	keyRegex   uintptr // char *
	valueRegex uintptr // char *
}

type wafResult struct {
	timeout       byte
	events        wafObject
	actions       wafObject
	derivatives   wafObject
	total_runtime uint64
}

type wafRulesetInfo struct {
	loaded  uint16
	failed  uint16
	errors  wafObject
	version uintptr // char *
}

// wafHandle is a forward declaration in ddwaf.h header
// We basically don't need to modify it, only to give it to the waf
type wafHandle uintptr

// wafContext is a forward declaration in ddwaf.h header
// We basically don't need to modify it, only to give it to the waf
type wafContext uintptr

// gostring copies a char* to a Go string.
func gostring(ptr *byte) string {
	if ptr == nil {
		return ""
	}
	var length int
	for {
		if *(*byte)(unsafe.Add(unsafe.Pointer(ptr), uintptr(length))) == '\x00' {
			break
		}
		length++
	}
	//string builtin copies the slice
	return string(unsafe.Slice(ptr, length))
}

// nativeStringUnwrap cast a native string type into it's runtime value. Exported as the struct reflect.StringHeader
func nativeStringUnwrap(str string) reflect.StringHeader {
	return *(*reflect.StringHeader)(unsafe.Pointer(&str))
}

func gostringSized(ptr *byte, size uint64) string {
	if ptr == nil {
		return ""
	}
	return string(unsafe.Slice(ptr, size))
}

// cstring converts a go string to *byte that can be passed to C code.
func cstring(name string) *byte {
	var b = make([]byte, len(name)+1)
	copy(b, name)
	return &b[0]
}

// cast is used to centralize unsafe use C of allocated pointer.
// We take the address and then dereference it to trick go vet from creating a possible misuse of unsafe.Pointer
func cast[T any](ptr uintptr) *T {
	return (*T)(*(*unsafe.Pointer)(unsafe.Pointer(&ptr)))
}

// nativeToUintptr is a helper used by populate wafObject values
// with Go values
func nativeToUintptr[T any](x T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&x))
}

// uintToNative is a helper used retrieve Go values from an uintptr encoded
// value from a wafObject
func uintptrToNative[T any](x uintptr) T {
	return *(*T)(unsafe.Pointer(&x))
}

// castWithOffset is the same as cast but adding an offset to the pointer by a multiple of the size
// of the type pointed.
func castWithOffset[T any](ptr uintptr, offset uint64) *T {
	return (*T)(unsafe.Add(*(*unsafe.Pointer)(unsafe.Pointer(&ptr)), offset*uint64(unsafe.Sizeof(*new(T)))))
}

// ptrToUintptr is a helper to centralize of usage of unsafe.Pointer
// do not use this function to cast interfaces
func ptrToUintptr[T any](arg *T) uintptr {
	return uintptr(unsafe.Pointer(arg))
}

func sliceToUintptr[T any](arg []T) uintptr {
	return (*reflect.SliceHeader)(unsafe.Pointer(&arg)).Data
}

func stringToUintptr(arg string) uintptr {
	return (*reflect.StringHeader)(unsafe.Pointer(&arg)).Data
}

// keepAlive() globals
var (
	alwaysFalse bool
	escapeSink  any
)

// keepAlive is a copy of runtime.KeepAlive
// keepAlive has 2 usages:
// - It forces the deallocation of the memory to take place later than expected (just like runtime.KeepAlive)
// - It forces the given argument x to be escaped on the heap by saving it into a global value (Go doesn't provide a standard way to do it as of today)
// It is implemented so that the compiler cannot optimize it.
//
//go:noinline
func keepAlive[T any](x T) {
	if alwaysFalse {
		escapeSink = x
	}
}
