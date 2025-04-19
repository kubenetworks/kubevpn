// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

//go:build otel_workaround

package internal

import (
	// OTel did a breaking change to the module go.opentelemetry.io/collector/pdata which is imported by the agent
	// and go.opentelemetry.io/collector/pdata/pprofile depends on it and is breaking because of it
	// For some reason the dependency closure won't let use upgrade this module past the point where it does not break anymore
	// So we are forced to add a blank import of this module to give us back the control over its version
	//
	// TODO: remove this once github.com/datadog-agent/pkg/trace has upgraded both modules past the breaking change
	_ "go.opentelemetry.io/collector/pdata/pprofile"
)
