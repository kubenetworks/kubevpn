// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"github.com/DataDog/go-libddwaf/v2/errors"
	"github.com/DataDog/go-libddwaf/v2/internal/bindings"
	"github.com/DataDog/go-libddwaf/v2/internal/unsafe"
)

// decodeErrors transforms the wafObject received by the wafRulesetInfo after the call to wafDl.wafInit to a map where
// keys are the error message and the value is a array of all the rule ids which triggered this specific error
func decodeErrors(obj *bindings.WafObject) (map[string][]string, error) {
	if !obj.IsMap() {
		return nil, errors.ErrInvalidObjectType
	}

	if obj.Value == 0 && obj.NbEntries > 0 {
		return nil, errors.ErrNilObjectPtr
	}

	wafErrors := map[string][]string{}
	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)

		errorMessage := unsafe.GostringSized(unsafe.Cast[byte](objElem.ParameterName), objElem.ParameterNameLength)
		ruleIds, err := decodeStringArray(objElem)
		if err != nil {
			return nil, err
		}

		wafErrors[errorMessage] = ruleIds
	}

	return wafErrors, nil
}

func decodeDiagnostics(obj *bindings.WafObject) (*Diagnostics, error) {
	if !obj.IsMap() {
		return nil, errors.ErrInvalidObjectType
	}
	if obj.Value == 0 && obj.NbEntries > 0 {
		return nil, errors.ErrNilObjectPtr
	}

	var diags Diagnostics
	var err error
	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)
		key := unsafe.GostringSized(unsafe.Cast[byte](objElem.ParameterName), objElem.ParameterNameLength)
		switch key {
		case "custom_rules":
			diags.CustomRules, err = decodeDiagnosticsEntry(objElem)
		case "exclusions":
			diags.Exclusions, err = decodeDiagnosticsEntry(objElem)
		case "rules":
			diags.Rules, err = decodeDiagnosticsEntry(objElem)
		case "rules_data":
			diags.RulesData, err = decodeDiagnosticsEntry(objElem)
		case "rules_override":
			diags.RulesOverrides, err = decodeDiagnosticsEntry(objElem)
		case "processors":
			diags.Processors, err = decodeDiagnosticsEntry(objElem)
		case "scanners":
			diags.Scanners, err = decodeDiagnosticsEntry(objElem)
		case "ruleset_version":
			diags.Version = unsafe.GostringSized(unsafe.Cast[byte](objElem.Value), objElem.NbEntries)
		default:
			// ignore?
		}
		if err != nil {
			return nil, err
		}
	}

	return &diags, nil
}

func decodeDiagnosticsEntry(obj *bindings.WafObject) (*DiagnosticEntry, error) {
	if !obj.IsMap() {
		return nil, errors.ErrInvalidObjectType
	}
	if obj.Value == 0 && obj.NbEntries > 0 {
		return nil, errors.ErrNilObjectPtr
	}
	var entry DiagnosticEntry
	var err error

	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)
		key := unsafe.GostringSized(unsafe.Cast[byte](objElem.ParameterName), objElem.ParameterNameLength)
		switch key {
		case "addresses":
			entry.Addresses, err = decodeDiagnosticAddresses(objElem)
		case "error":
			entry.Error = unsafe.GostringSized(unsafe.Cast[byte](objElem.Value), objElem.NbEntries)
		case "errors":
			entry.Errors, err = decodeErrors(objElem)
		case "failed":
			entry.Failed, err = decodeStringArray(objElem)
		case "loaded":
			entry.Loaded, err = decodeStringArray(objElem)
		default:
			return nil, errors.ErrUnsupportedValue
		}

		if err != nil {
			return nil, err
		}
	}

	return &entry, nil
}

func decodeDiagnosticAddresses(obj *bindings.WafObject) (*DiagnosticAddresses, error) {
	if !obj.IsMap() {
		return nil, errors.ErrInvalidObjectType
	}
	if obj.Value == 0 && obj.NbEntries > 0 {
		return nil, errors.ErrNilObjectPtr
	}

	addrs := &DiagnosticAddresses{}

	var err error
	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)
		key := unsafe.GostringSized(unsafe.Cast[byte](objElem.ParameterName), objElem.ParameterNameLength)
		switch key {
		case "required":
			addrs.Required, err = decodeStringArray(objElem)
			if err != nil {
				return nil, err
			}
		case "optional":
			addrs.Optional, err = decodeStringArray(objElem)
			if err != nil {
				return nil, err
			}
		default:
			return nil, errors.ErrUnsupportedValue
		}
	}

	return addrs, nil
}

func decodeStringArray(obj *bindings.WafObject) ([]string, error) {
	// We consider that nil is an empty array
	if obj.IsNil() {
		return nil, nil
	}

	if !obj.IsArray() {
		return nil, errors.ErrInvalidObjectType
	}

	if obj.Value == 0 && obj.NbEntries > 0 {
		return nil, errors.ErrNilObjectPtr
	}

	var strArr []string
	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)
		if objElem.Type != bindings.WafStringType {
			return nil, errors.ErrInvalidObjectType
		}

		strArr = append(strArr, unsafe.GostringSized(unsafe.Cast[byte](objElem.Value), objElem.NbEntries))
	}

	return strArr, nil
}

func decodeObject(obj *bindings.WafObject) (any, error) {
	switch obj.Type {
	case bindings.WafMapType:
		return decodeMap(obj)
	case bindings.WafArrayType:
		return decodeArray(obj)
	case bindings.WafStringType:
		return unsafe.GostringSized(unsafe.Cast[byte](obj.Value), obj.NbEntries), nil
	case bindings.WafIntType:
		return int64(obj.Value), nil
	case bindings.WafUintType:
		return uint64(obj.Value), nil
	case bindings.WafFloatType:
		return unsafe.UintptrToNative[float64](obj.Value), nil
	case bindings.WafBoolType:
		return unsafe.UintptrToNative[bool](obj.Value), nil
	case bindings.WafNilType:
		return nil, nil
	default:
		return nil, errors.ErrUnsupportedValue
	}
}

func decodeArray(obj *bindings.WafObject) ([]any, error) {
	if obj.IsNil() {
		return nil, nil
	}

	if !obj.IsArray() {
		return nil, errors.ErrInvalidObjectType
	}

	events := make([]any, obj.NbEntries)

	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)
		val, err := decodeObject(objElem)
		if err != nil {
			return nil, err
		}
		events[i] = val
	}

	return events, nil
}

func decodeMap(obj *bindings.WafObject) (map[string]any, error) {
	if obj.IsNil() {
		return nil, nil
	}

	if !obj.IsMap() {
		return nil, errors.ErrInvalidObjectType
	}

	result := make(map[string]any, obj.NbEntries)
	for i := uint64(0); i < obj.NbEntries; i++ {
		objElem := unsafe.CastWithOffset[bindings.WafObject](obj.Value, i)
		key := unsafe.GostringSized(unsafe.Cast[byte](objElem.ParameterName), objElem.ParameterNameLength)
		val, err := decodeObject(objElem)
		if err != nil {
			return nil, err
		}
		result[key] = val
	}

	return result, nil
}
