// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

// decodeErrors transforms the wafObject received by the wafRulesetInfo after the call to wafDl.wafInit to a map where
// keys are the error message and the value is a array of all the rule ids which triggered this specific error
func decodeErrors(obj *wafObject) (map[string][]string, error) {
	if !obj.isMap() {
		return nil, errInvalidObjectType
	}

	if obj.value == 0 && obj.nbEntries > 0 {
		return nil, errNilObjectPtr
	}

	wafErrors := map[string][]string{}
	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)

		errorMessage := gostringSized(cast[byte](objElem.parameterName), objElem.parameterNameLength)
		ruleIds, err := decodeStringArray(objElem)
		if err != nil {
			return nil, err
		}

		wafErrors[errorMessage] = ruleIds
	}

	return wafErrors, nil
}

func decodeDiagnostics(obj *wafObject) (*Diagnostics, error) {
	if !obj.isMap() {
		return nil, errInvalidObjectType
	}
	if obj.value == 0 && obj.nbEntries > 0 {
		return nil, errNilObjectPtr
	}

	var diags Diagnostics
	var err error
	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)
		key := gostringSized(cast[byte](objElem.parameterName), objElem.parameterNameLength)
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
			diags.Version = gostringSized(cast[byte](objElem.value), objElem.nbEntries)
		default:
			// ignore?
		}
		if err != nil {
			return nil, err
		}
	}

	return &diags, nil
}

func decodeDiagnosticsEntry(obj *wafObject) (*DiagnosticEntry, error) {
	if !obj.isMap() {
		return nil, errInvalidObjectType
	}
	if obj.value == 0 && obj.nbEntries > 0 {
		return nil, errNilObjectPtr
	}
	var entry DiagnosticEntry
	var err error

	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)
		key := gostringSized(cast[byte](objElem.parameterName), objElem.parameterNameLength)
		switch key {
		case "addresses":
			entry.Addresses, err = decodeDiagnosticAddresses(objElem)
		case "error":
			entry.Error = gostringSized(cast[byte](objElem.value), objElem.nbEntries)
		case "errors":
			entry.Errors, err = decodeErrors(objElem)
		case "failed":
			entry.Failed, err = decodeStringArray(objElem)
		case "loaded":
			entry.Loaded, err = decodeStringArray(objElem)
		default:
			return nil, errUnsupportedValue
		}

		if err != nil {
			return nil, err
		}
	}

	return &entry, nil
}

func decodeDiagnosticAddresses(obj *wafObject) (*DiagnosticAddresses, error) {
	if !obj.isMap() {
		return nil, errInvalidObjectType
	}
	if obj.value == 0 && obj.nbEntries > 0 {
		return nil, errNilObjectPtr
	}

	addrs := &DiagnosticAddresses{}

	var err error
	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)
		key := gostringSized(cast[byte](objElem.parameterName), objElem.parameterNameLength)
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
			return nil, errUnsupportedValue
		}
	}

	return addrs, nil
}

func decodeStringArray(obj *wafObject) ([]string, error) {
	// We consider that nil is an empty array
	if obj.isNil() {
		return nil, nil
	}

	if !obj.isArray() {
		return nil, errInvalidObjectType
	}

	if obj.value == 0 && obj.nbEntries > 0 {
		return nil, errNilObjectPtr
	}

	var strArr []string
	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)
		if objElem._type != wafStringType {
			return nil, errInvalidObjectType
		}

		strArr = append(strArr, gostringSized(cast[byte](objElem.value), objElem.nbEntries))
	}

	return strArr, nil
}

func decodeObject(obj *wafObject) (any, error) {
	switch obj._type {
	case wafMapType:
		return decodeMap(obj)
	case wafArrayType:
		return decodeArray(obj)
	case wafStringType:
		return gostringSized(cast[byte](obj.value), obj.nbEntries), nil
	case wafIntType:
		return int64(obj.value), nil
	case wafUintType:
		return uint64(obj.value), nil
	case wafFloatType:
		return uintptrToNative[float64](obj.value), nil
	case wafBoolType:
		return uintptrToNative[bool](obj.value), nil
	case wafNilType:
		return nil, nil
	default:
		return nil, errUnsupportedValue
	}
}

func decodeArray(obj *wafObject) ([]any, error) {
	if obj.isNil() {
		return nil, nil
	}

	if !obj.isArray() {
		return nil, errInvalidObjectType
	}

	events := make([]any, obj.nbEntries)

	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)
		val, err := decodeObject(objElem)
		if err != nil {
			return nil, err
		}
		events[i] = val
	}

	return events, nil
}

func decodeMap(obj *wafObject) (map[string]any, error) {
	if obj.isNil() {
		return nil, nil
	}

	if !obj.isMap() {
		return nil, errInvalidObjectType
	}

	result := make(map[string]any, obj.nbEntries)
	for i := uint64(0); i < obj.nbEntries; i++ {
		objElem := castWithOffset[wafObject](obj.value, i)
		key := gostringSized(cast[byte](objElem.parameterName), objElem.parameterNameLength)
		val, err := decodeObject(objElem)
		if err != nil {
			return nil, err
		}
		result[key] = val
	}

	return result, nil
}
