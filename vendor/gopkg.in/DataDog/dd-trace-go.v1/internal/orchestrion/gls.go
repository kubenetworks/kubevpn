// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package orchestrion

import (
	_ "runtime" // to make sure the symbols we link to are present
	_ "unsafe"  // for go:linkname
)

var (
	// getDDGLS returns the current value from the field inserted in runtime.g by orchestrion.
	// Or nil if orchestrion is not enabled.
	getDDGLS = func() any { return nil }
	// setDDGLS sets the value in the field inserted in runtime.g by orchestrion.
	// Or does nothing if orchestrion is not enabled.
	setDDGLS = func(any) {}
)

// Accessors set by orchestrion in the runtime package. If orchestrion is not enabled, these will be nil as per the default values.

//revive:disable:var-naming
//go:linkname __dd_orchestrion_gls_get __dd_orchestrion_gls_get
var __dd_orchestrion_gls_get func() any

//go:linkname __dd_orchestrion_gls_set __dd_orchestrion_gls_set
var __dd_orchestrion_gls_set func(any)

//revive:enable:var-naming

// Check at Go init time that the two function variable values created by the
// orchestrion are present, and set the get/set variables to their
// values.
func init() {
	if __dd_orchestrion_gls_get != nil && __dd_orchestrion_gls_set != nil {
		getDDGLS = __dd_orchestrion_gls_get
		setDDGLS = __dd_orchestrion_gls_set
	}
}
