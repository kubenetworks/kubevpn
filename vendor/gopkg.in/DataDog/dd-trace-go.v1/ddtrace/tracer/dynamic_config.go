// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"sync"
)

// dynamicConfig is a thread-safe generic data structure to represent configuration fields.
// It's designed to satisfy the dynamic configuration semantics (i.e reset, update, apply configuration changes).
// This structure will be extended to track the origin of configuration values as well (e.g remote_config, env_var).
type dynamicConfig[T any] struct {
	sync.RWMutex
	current T       // holds the current configuration value
	startup T       // holds the startup configuration value
	apply   func(T) // applies a configuration value
	isReset bool    // internal boolean to avoid unnecessary resets
}

func newDynamicConfig[T any](val T, apply func(T)) dynamicConfig[T] {
	return dynamicConfig[T]{
		current: val,
		startup: val,
		apply:   apply,
		isReset: true,
	}
}

// update applies a new configuration value
func (dc *dynamicConfig[T]) update(val T) {
	dc.Lock()
	defer dc.Unlock()
	dc.current = val
	dc.apply(val)
	dc.isReset = false
}

// reset re-applies the startup configuration value
func (dc *dynamicConfig[T]) reset() {
	dc.Lock()
	defer dc.Unlock()
	if dc.isReset {
		return
	}
	dc.current = dc.startup
	dc.apply(dc.startup)
	dc.isReset = true
}

// handleRC processes a new configuration value from remote config
func (dc *dynamicConfig[T]) handleRC(val *T) {
	if val != nil {
		dc.update(*val)
		return
	}
	dc.reset()
}
