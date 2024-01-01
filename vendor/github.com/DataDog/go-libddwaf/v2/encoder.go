// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"math"
	"reflect"
	"strings"
	"unicode"
)

// Encode Go values into wafObjects. Only the subset of Go types representable into wafObjects
// will be encoded while ignoring the rest of it.
// The encoder allocates the memory required for new wafObjects into the Go memory, which must be kept
// referenced for their lifetime in the C world. This lifetime depends on the ddwaf function being used with.
// the encoded result. The Go references of the allocated wafObjects, along with every Go pointer they may
// reference now or in the future, are stored and referenced in the `cgoRefs` field. The user MUST leverage
// `keepAlive()` with it according to its ddwaf use-case.
type encoder struct {
	cgoRefs          cgoRefPool
	containerMaxSize int
	stringMaxSize    int
	objectMaxDepth   int
}

type native interface {
	int64 | uint64 | uintptr
}

func newLimitedEncoder() encoder {
	return encoder{
		containerMaxSize: wafMaxContainerSize,
		stringMaxSize:    wafMaxStringLength,
		objectMaxDepth:   wafMaxContainerDepth,
	}
}

func newMaxEncoder() encoder {
	return encoder{
		containerMaxSize: math.MaxInt,
		stringMaxSize:    math.MaxInt,
		objectMaxDepth:   math.MaxInt,
	}
}

// Encode takes a Go value and returns a wafObject pointer and an error.
// The returned wafObject is the root of the tree of nested wafObjects representing the Go value.
// The only error case is if the top-level object is "Unusable" which means that the data is nil or a non-data type
// like a function or a channel.
func (encoder *encoder) Encode(data any) (*wafObject, error) {
	value := reflect.ValueOf(data)
	wo := &wafObject{}

	if err := encoder.encode(value, wo, encoder.objectMaxDepth); err != nil {
		return nil, err
	}

	return wo, nil
}

// EncodeAddresses takes a map of Go values and returns a wafObject pointer and an error.
// The returned wafObject is the root of the tree of nested wafObjects representing the Go values.
// This function is further optimized from Encode to take addresses as input and avoid further
// errors in case the top-level map with addresses as keys is nil.
// Since errors returned by Encode are not sent up between levels of the tree, this means that all errors come from the
// top layer of encoding, which is the map of addresses. Hence, all errors should be developer errors since the map of
// addresses is not user defined custom data.
func (encoder *encoder) EncodeAddresses(addresses map[string]any) (*wafObject, error) {
	if addresses == nil {
		return nil, errUnsupportedValue
	}

	return encoder.Encode(addresses)
}

func encodeNative[T native](val T, t wafObjectType, obj *wafObject) {
	obj._type = t
	obj.value = (uintptr)(val)
}

var nullableTypeKinds = map[reflect.Kind]struct{}{
	reflect.Interface:     {},
	reflect.Pointer:       {},
	reflect.UnsafePointer: {},
	reflect.Map:           {},
	reflect.Slice:         {},
	reflect.Func:          {},
	reflect.Chan:          {},
}

// isValueNil check if the value is nullable and if it is actually nil
// we cannot directly use value.IsNil() because it panics on non-pointer values
func isValueNil(value reflect.Value) bool {
	_, nullable := nullableTypeKinds[value.Kind()]
	return nullable && value.IsNil()
}

func (encoder *encoder) encode(value reflect.Value, obj *wafObject, depth int) error {
	switch kind := value.Kind(); {
	// Terminal cases (leafs of the tree)

	//		Is invalid type: nil interfaces for example, cannot be used to run any reflect method or it's susceptible to panic
	case !value.IsValid() || kind == reflect.Invalid:
		return errUnsupportedValue
	// 		Is nullable type: nil pointers, channels, maps or functions
	case isValueNil(value):
		encodeNative[uintptr](0, wafNilType, obj)

	// 		Booleans
	case kind == reflect.Bool:
		encodeNative(nativeToUintptr(value.Bool()), wafBoolType, obj)

	// 		Numbers
	case value.CanInt(): // any int type or alias
		encodeNative(value.Int(), wafIntType, obj)
	case value.CanUint(): // any Uint type or alias
		encodeNative(value.Uint(), wafUintType, obj)
	case value.CanFloat(): // any float type or alias
		encodeNative(nativeToUintptr(value.Float()), wafFloatType, obj)

	//		Strings
	case kind == reflect.String: // string type
		encoder.encodeString(value.String(), obj)
	case value.Type() == reflect.TypeOf([]byte(nil)): // byte array -> string
		encoder.encodeString(string(value.Bytes()), obj)

	// 		Pointer and interfaces are not taken into account, we only recurse on them
	case kind == reflect.Interface || kind == reflect.Pointer:
		return encoder.encode(value.Elem(), obj, depth)

	// Containers (internal nodes of the tree)

	// 		All recursive cases can only execute if the depth is superior to 0
	case depth <= 0:
		return errMaxDepthExceeded

	// 		Either an array or a slice of an array
	case kind == reflect.Array || kind == reflect.Slice:
		encoder.encodeArray(value, obj, depth-1)
	case kind == reflect.Map:
		encoder.encodeMap(value, obj, depth-1)
	case kind == reflect.Struct:
		encoder.encodeStruct(value, obj, depth-1)

	default:
		return errUnsupportedValue
	}

	return nil
}

func (encoder *encoder) encodeString(str string, obj *wafObject) {
	if len(str) > encoder.stringMaxSize {
		str = str[:encoder.stringMaxSize]
	}

	encoder.cgoRefs.AllocWafString(obj, str)
}

func getFieldNameFromType(field reflect.StructField) (string, bool) {
	fieldName := field.Name

	// Private and synthetics fields
	if len(fieldName) < 1 || unicode.IsLower(rune(fieldName[0])) {
		return "", false
	}

	// Use the json tag name as field name if present
	if tag, ok := field.Tag.Lookup("json"); ok {
		if i := strings.IndexByte(tag, byte(',')); i > 0 {
			tag = tag[:i]
		}
		if len(tag) > 0 {
			fieldName = tag
		}
	}

	return fieldName, true
}

// encodeStruct takes a reflect.Value and a wafObject pointer and iterates on the struct field to build
// a wafObject map of type wafMapType. The specificities are the following:
// - It will only take the first encoder.containerMaxSize elements of the struct
// - If the field has a json tag it will become the field name
// - Private fields and also values producing an error at encoding will be skipped
// - Even if the element values are invalid or null we still keep them to report the field name
func (encoder *encoder) encodeStruct(value reflect.Value, obj *wafObject, depth int) {
	typ := value.Type()
	nbFields := typ.NumField()
	capacity := nbFields
	length := 0
	if capacity > encoder.containerMaxSize {
		capacity = encoder.containerMaxSize
	}

	objArray := encoder.cgoRefs.AllocWafArray(obj, wafMapType, uint64(capacity))
	for i := 0; length < capacity && i < nbFields; i++ {
		fieldType := typ.Field(i)
		fieldName, usable := getFieldNameFromType(fieldType)
		if !usable {
			continue
		}

		objElem := &objArray[length]
		// If the Map key is of unsupported type, skip it
		if encoder.encodeMapKey(reflect.ValueOf(fieldName), objElem, depth) != nil {
			continue
		}

		if err := encoder.encode(value.Field(i), objElem, depth); err != nil {
			// We still need to keep the map key, so we can't discard the full object, instead, we make the value a noop
			encodeNative[uintptr](0, wafInvalidType, objElem)
		}

		length++
	}

	// Set the length to the final number of successfully encoded elements
	obj.nbEntries = uint64(length)
}

// encodeMap takes a reflect.Value and a wafObject pointer and iterates on the map elements and returns
// a wafObject map of type wafMapType. The specificities are the following:
// - It will only take the first encoder.containerMaxSize elements of the map
// - Even if the element values are invalid or null we still keep them to report the map key
func (encoder *encoder) encodeMap(value reflect.Value, obj *wafObject, depth int) {
	capacity := value.Len()
	length := 0
	if capacity > encoder.containerMaxSize {
		capacity = encoder.containerMaxSize
	}

	objArray := encoder.cgoRefs.AllocWafArray(obj, wafMapType, uint64(capacity))
	for iter := value.MapRange(); iter.Next(); {
		if length == capacity {
			break
		}

		objElem := &objArray[length]
		if encoder.encodeMapKey(iter.Key(), objElem, depth) != nil {
			continue
		}

		if err := encoder.encode(iter.Value(), objElem, depth); err != nil {
			// We still need to keep the map key, so we can't discard the full object, instead, we make the value a noop
			encodeNative[uintptr](0, wafInvalidType, objElem)
		}
		length++
	}

	// Fix the size because we skipped map entries
	obj.nbEntries = uint64(length)
}

// encodeMapKey takes a reflect.Value and a wafObject and returns a wafObject ready to be considered a map key
// We use the function cgoRefPool.AllocWafMapKey to store the key in the wafObject. But first we need
// to grab the real underlying value by recursing through the pointer and interface values.
func (encoder *encoder) encodeMapKey(value reflect.Value, obj *wafObject, depth int) error {
	kind := value.Kind()
	for ; depth > 0 && (kind == reflect.Pointer || kind == reflect.Interface); value, kind = value.Elem(), value.Elem().Kind() {
		if value.IsNil() {
			return errInvalidMapKey
		}

		depth--
	}

	var keyStr string
	switch {
	case kind == reflect.Invalid:
		return errInvalidMapKey
	case kind == reflect.String:
		keyStr = value.String()
	case value.Type() == reflect.TypeOf([]byte(nil)):
		keyStr = string(value.Bytes())
	default:
		return errInvalidMapKey
	}

	if len(keyStr) > encoder.stringMaxSize {
		keyStr = keyStr[:encoder.stringMaxSize]
	}

	encoder.cgoRefs.AllocWafMapKey(obj, keyStr)
	return nil
}

// encodeArray takes a reflect.Value and a wafObject pointer and iterates on the elements and returns
// a wafObject array of type wafArrayType. The specificities are the following:
// - It will only take the first encoder.containerMaxSize elements of the array
// - Elements producing an error at encoding or null values will be skipped
func (encoder *encoder) encodeArray(value reflect.Value, obj *wafObject, depth int) {
	length := value.Len()
	capacity := length
	if capacity > encoder.containerMaxSize {
		capacity = encoder.containerMaxSize
	}

	currIndex := 0
	objArray := encoder.cgoRefs.AllocWafArray(obj, wafArrayType, uint64(capacity))
	for i := 0; currIndex < capacity && i < length; i++ {
		objElem := &objArray[currIndex]

		if err := encoder.encode(value.Index(i), objElem, depth); err != nil {
			continue
		}

		// If the element is null or invalid it has no impact on the waf execution, therefore we can skip its
		// encoding. In this specific case we just overwrite it at the next loop iteration.
		if objElem.IsUnusable() {
			continue
		}

		currIndex++
	}

	// Fix the size because we skipped map entries
	obj.nbEntries = uint64(currIndex)
}
