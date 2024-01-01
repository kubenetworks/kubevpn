// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package traceprof

import (
	"sync/atomic"
)

var profiler struct {
	enabled uint32
}

func SetProfilerEnabled(val bool) bool {
	return atomic.SwapUint32(&profiler.enabled, boolToUint32(val)) != 0
}

func profilerEnabled() int {
	return int(atomic.LoadUint32(&profiler.enabled))
}

func boolToUint32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func SetProfilerRootTags(localRootSpan TagSetter) {
	localRootSpan.SetTag("_dd.profiling.enabled", profilerEnabled())
}

type TagSetter interface{ SetTag(string, interface{}) }
