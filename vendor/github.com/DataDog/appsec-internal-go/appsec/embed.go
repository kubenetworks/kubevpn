// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package appsec

import _ "embed" // Blank import comment for golint compliance

// StaticRecommendedRules holds the recommended AppSec security rules (v1.13.2)
// Source: https://github.com/DataDog/appsec-event-rules/blob/1.13.2/build/recommended.json
//
//go:embed rules.json
var StaticRecommendedRules string
