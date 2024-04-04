// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// The Go build tag "appsec" was introduced to avoid having CGO_ENABLED=0 breaking changes
// due to purego's dynamic link against libdl.so, which is not expected when CGO is disabled.
//go:build !cgo && !appsec

package support

import "github.com/DataDog/go-libddwaf/v2/errors"

func init() {
	wafSupportErrors = append(wafSupportErrors, errors.CgoDisabledError{})
}
