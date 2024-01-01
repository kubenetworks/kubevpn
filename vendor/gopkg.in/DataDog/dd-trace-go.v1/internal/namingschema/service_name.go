// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package namingschema

import "gopkg.in/DataDog/dd-trace-go.v1/internal/globalconfig"

// NewDefaultServiceName returns a Schema with the standard logic to be used for contrib span service names
// (in-code override > DD_SERVICE environment variable > integration default name).
// If you need to support older versions not following this logic, you can use WithV0Override option to override this behavior.
func NewDefaultServiceName(fallbackName string, opts ...Option) *Schema {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return New(&standardServiceNameSchema{
		fallbackName:         fallbackName,
		useGlobalServiceName: UseGlobalServiceName(),
		cfg:                  cfg,
	})
}

type standardServiceNameSchema struct {
	fallbackName         string
	useGlobalServiceName bool
	cfg                  *config
}

func (s *standardServiceNameSchema) V0() string {
	// the override function for V0 is used by contribs to introduce their default service names (i.e. "kafka, mongo, etc.")
	// when V0 is used. The extra flag useGlobalServiceName allows to disable these default service names even when V0
	// is used.
	if s.cfg.overrideV0 == nil || s.useGlobalServiceName {
		return s.getName()
	}
	return *s.cfg.overrideV0
}

func (s *standardServiceNameSchema) V1() string {
	return s.getName()
}

func (s *standardServiceNameSchema) getName() string {
	if svc := globalconfig.ServiceName(); svc != "" {
		return svc
	}
	return s.fallbackName
}
