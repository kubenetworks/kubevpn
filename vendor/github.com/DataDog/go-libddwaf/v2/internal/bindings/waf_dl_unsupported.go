// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Build when the target OS or architecture are not supported
//go:build (!linux && !darwin) || (!amd64 && !arm64) || go1.23 || datadog.no_waf || (!cgo && !appsec)

package bindings

type WafDl struct{}

func NewWafDl() (dl *WafDl, err error) {
	return nil, nil
}

func (waf *WafDl) WafGetVersion() string {
	return ""
}

func (waf *WafDl) WafInit(obj *WafObject, config *WafConfig, info *WafObject) WafHandle {
	return 0
}

func (waf *WafDl) WafUpdate(handle WafHandle, ruleset *WafObject, info *WafObject) WafHandle {
	return 0
}

func (waf *WafDl) WafDestroy(handle WafHandle) {
}

func (waf *WafDl) WafKnownAddresses(handle WafHandle) []string {
	return nil
}

func (waf *WafDl) WafContextInit(handle WafHandle) WafContext {
	return 0
}

func (waf *WafDl) WafContextDestroy(context WafContext) {
}

func (waf *WafDl) WafResultFree(result *WafResult) {
}

func (waf *WafDl) WafObjectFree(obj *WafObject) {
}

func (waf *WafDl) WafRun(context WafContext, persistentData, ephemeralData *WafObject, result *WafResult, timeout uint64) WafReturnCode {
	return WafErrInternal
}
