// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package listener

import (
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/config"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
)

// Feature is an interface that represents a feature that can be started and stopped.
type Feature interface {
	// String should return a user-friendly name for the feature.
	String() string
	// Stop stops the feature.
	Stop()
}

// NewFeature is a function that creates a new feature.
// The error returned will be fatal for the application if not nil.
// If both the feature and the error are nil, the feature will be considered inactive.
type NewFeature func(*config.Config, dyngo.Operation) (Feature, error)
