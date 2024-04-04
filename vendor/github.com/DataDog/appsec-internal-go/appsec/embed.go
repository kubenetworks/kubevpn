// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package appsec

import _ "embed" // Blank import comment for golint compliance

// StaticRecommendedRules holds the recommended AppSec security rules (v1.11.0)
// Source: https://github.com/DataDog/appsec-event-rules/blob/1.11.0/build/recommended.json
//
//go:embed rules.json
var StaticRecommendedRules string

// StaticProcessors holds the default processors and scanners used for API Security
// Not part of the recommended security rules
//
//go:embed processors.json
var StaticProcessors string
