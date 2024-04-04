// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"strconv"

	"github.com/DataDog/go-libddwaf/v2/internal/bindings"
	"github.com/DataDog/go-libddwaf/v2/internal/unsafe"
)

// cgoRefPool is a way to make sure we can safely send go allocated data on the C side of the WAF
// The main issue is the following: the wafObject uses a C union to store the tree structure of the full object,
// union equivalent in go are interfaces and they are not compatible with C unions. The only way to be 100% sure
// that the Go wafObject struct have the same layout as the C one is to only use primitive types. So the only way to
// store a raw pointer is to use the uintptr type. But since uintptr do not have pointer semantics (and are just
// basically integers), we need another structure to store the value as Go pointer because the GC is lurking. That's
// where the cgoRefPool object comes into play: All new wafObject elements are created via this API whose especially
// built to make sure there is no gap for the Garbage Collector to exploit. From there, since underlying values of the
// wafObject are either arrays (for maps, structs and arrays) or string (for all ints, booleans and strings),
// we can store 2 slices of arrays and use runtime.KeepAlive in each code path to protect them from the GC.
type cgoRefPool struct {
	stringRefs []string
	arrayRefs  [][]bindings.WafObject
}

func (refPool *cgoRefPool) append(newRefs cgoRefPool) {
	refPool.stringRefs = append(refPool.stringRefs, newRefs.stringRefs...)
	refPool.arrayRefs = append(refPool.arrayRefs, newRefs.arrayRefs...)
}

// AllocCString is used in the rare cases where we need the WAF to receive standard null-terminated strings.
// All cases where strings a wrapped in wafObject are handled by AllocWafString
func (refPool *cgoRefPool) AllocCString(str string) uintptr {
	if len(str) > 0 && str[len(str)-1] != '\x00' {
		str = str + "\x00"
	}

	refPool.stringRefs = append(refPool.stringRefs, str)
	return unsafe.NativeStringUnwrap(str).Data
}

// AllocWafString fills the obj parameter wafObject with all parameters needed for the WAF interpret it as a string.
// We take full advantage of the fact that the WAF can receive non-null-terminated strings by directly retrieving the
// underlying array in the string value using the nativeStringUnwrap function. Hence, removing any copy in the process
func (refPool *cgoRefPool) AllocWafString(obj *bindings.WafObject, str string) {
	obj.Type = bindings.WafStringType

	if len(str) == 0 {
		obj.NbEntries = 0
		obj.Value = 0
		return
	}

	refPool.stringRefs = append(refPool.stringRefs, str)
	stringHeader := unsafe.NativeStringUnwrap(str)
	obj.Value = stringHeader.Data
	obj.NbEntries = uint64(stringHeader.Len)
}

// AllocWafArray is used to create a tree-like structure since we allocate a wafObject array inside another wafOject.
// wafObject can also represent a map, in that case we use the AllocWafMapKey function to make the wafObject key-value-pair
// like objects.
func (refPool *cgoRefPool) AllocWafArray(obj *bindings.WafObject, typ bindings.WafObjectType, size uint64) []bindings.WafObject {
	if typ != bindings.WafMapType && typ != bindings.WafArrayType {
		panic("Cannot allocate this waf object data type as an array: " + strconv.Itoa(int(typ)))
	}

	obj.Type = typ
	obj.NbEntries = size

	// If the array size is zero no need to allocate anything
	if size == 0 {
		obj.Value = 0
		return nil
	}

	goArray := make([]bindings.WafObject, size)
	refPool.arrayRefs = append(refPool.arrayRefs, goArray)

	obj.Value = unsafe.SliceToUintptr(goArray)
	return goArray
}

// AllocWafMapKey is used to store a string map key in a wafObject.
// We take full advantage of the fact that the WAF can receive non-null-terminated strings by directly retrieving the
// underlying array in the string value using the nativeStringUnwrap function. Hence, removing any copy in the process
func (refPool *cgoRefPool) AllocWafMapKey(obj *bindings.WafObject, str string) {
	if len(str) == 0 {
		return
	}

	refPool.stringRefs = append(refPool.stringRefs, str)
	stringHeader := unsafe.NativeStringUnwrap(str)
	obj.ParameterName = stringHeader.Data
	obj.ParameterNameLength = uint64(stringHeader.Len)
}
