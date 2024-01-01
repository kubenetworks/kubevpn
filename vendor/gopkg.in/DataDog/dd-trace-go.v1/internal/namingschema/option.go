// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package namingschema

// Option represents an option that can be passed to some naming schemas provided in this package.
type Option func(cfg *config)

type config struct {
	overrideV0 *string
}

// WithOverrideV0 allows to override the value returned for V0 in the given Schema.
func WithOverrideV0(value string) Option {
	return func(cfg *config) {
		cfg.overrideV0 = &value
	}
}
