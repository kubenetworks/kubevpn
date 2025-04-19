// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package tracer

import (
	"math"
	"math/rand/v2"
)

func randUint64() uint64 {
	return rand.Uint64()
}

func generateSpanID(startTime int64) uint64 {
	return rand.Uint64() & math.MaxInt64
}
