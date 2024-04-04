// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

//go:build cgo

package appsec

func init() {
	// Go doesn't provide any way to check if cgo is enabled, so we compute it
	// ourselves with the cgo build tag.
	cgoEnabled = true
}
