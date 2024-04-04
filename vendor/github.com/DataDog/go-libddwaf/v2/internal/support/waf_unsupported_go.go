// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Unsupported Go versions (>=)
//go:build go1.23

package support

import "github.com/DataDog/go-libddwaf/v2/errors"

func init() {
	wafSupportErrors = append(wafSupportErrors, errors.UnsupportedGoVersionError{})
}
