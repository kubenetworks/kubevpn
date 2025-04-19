// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package orchestrion

//go:generate go run ./generator

// Orchestrion will change this at build-time
//
//dd:orchestrion-enabled
var enabled = false

// Enabled returns whether the current build was compiled with orchestrion or not.
func Enabled() bool {
	return enabled
}
