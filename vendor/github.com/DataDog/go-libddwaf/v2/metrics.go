// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"fmt"
	"sync"
	"time"
)

// Stats stores the metrics collected by the WAF.
type Stats struct {
	// Timers returns a map of metrics and their durations.
	Timers map[string]time.Duration

	// Timeout
	TimeoutCount uint64

	// Truncations provides details about truncations that occurred while
	// encoding address data for WAF execution.
	Truncations map[TruncationReason][]int
}

const (
	wafEncodeTag     = "_dd.appsec.waf.encode"
	wafRunTag        = "_dd.appsec.waf.duration_ext"
	wafDurationTag   = "_dd.appsec.waf.duration"
	wafDecodeTag     = "_dd.appsec.waf.decode"
	wafTimeoutTag    = "_dd.appsec.waf.timeouts"
	wafTruncationTag = "_dd.appsec.waf.truncations"
)

// Metrics transform the stats returned by the WAF into a map of key value metrics for datadog backend
func (stats Stats) Metrics() map[string]any {
	tags := make(map[string]any, len(stats.Timers)+len(stats.Truncations)+1)
	for k, v := range stats.Timers {
		tags[k] = float64(v.Nanoseconds()) / float64(time.Microsecond) // The metrics should be in microseconds
	}

	tags[wafTimeoutTag] = stats.TimeoutCount
	for reason, list := range stats.Truncations {
		tags[fmt.Sprintf("%s.%s", wafTruncationTag, reason.String())] = list
	}

	return tags
}

type metricsStore struct {
	data  map[string]time.Duration
	mutex sync.RWMutex
}

func (metrics *metricsStore) add(key string, duration time.Duration) {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	if metrics.data == nil {
		metrics.data = make(map[string]time.Duration, 5)
	}

	metrics.data[key] += duration
}

func (metrics *metricsStore) get(key string) time.Duration {
	metrics.mutex.RLock()
	defer metrics.mutex.RUnlock()
	return metrics.data[key]
}

func (metrics *metricsStore) copy() map[string]time.Duration {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	if metrics.data == nil {
		return nil
	}

	copy := make(map[string]time.Duration, len(metrics.data))
	for k, v := range metrics.data {
		copy[k] = v
	}
	return copy
}

// merge merges the current metrics with new ones
func (metrics *metricsStore) merge(other map[string]time.Duration) {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	if metrics.data == nil {
		metrics.data = make(map[string]time.Duration, 5)
	}

	for key, val := range other {
		prev, ok := metrics.data[key]
		if !ok {
			prev = 0
		}
		metrics.data[key] = prev + val
	}
}
