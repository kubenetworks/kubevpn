// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Unsupported target OS or architecture
//            Unsupported OS        Unsupported Arch
//go:build (!linux && !darwin) || (!amd64 && !arm64)

package waf

import (
	"runtime"
)

func init() {
	wafSupportErrors = append(wafSupportErrors, UnsupportedOSArchError{runtime.GOOS, runtime.GOARCH})
}
