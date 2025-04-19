// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package timer

import (
	"sync/atomic"
)

// components store the data shared between child timers of the same component name
type components struct {
	lookup  map[string]*atomic.Int64
	storage []atomic.Int64
}

func newComponents(names []string) components {
	lookup := make(map[string]*atomic.Int64, len(names))
	storage := make([]atomic.Int64, len(names))
	for i, name := range names {
		lookup[name] = &storage[i]
	}
	return components{
		lookup:  lookup,
		storage: storage,
	}
}
