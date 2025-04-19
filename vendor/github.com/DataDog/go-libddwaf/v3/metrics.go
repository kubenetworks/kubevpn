// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package waf

import (
	"strings"
	"sync"
	"time"
)

// Stats stores the metrics collected by the WAF.
type (
	Stats struct {
		// Timers returns a map of metrics and their durations.
		Timers map[string]time.Duration

		// TimeoutCount for the Default Scope i.e. "waf"
		TimeoutCount uint64

		// TimeoutRASPCount for the RASP Scope i.e. "rasp"
		TimeoutRASPCount uint64

		// Truncations provides details about truncations that occurred while
		// encoding address data for WAF execution.
		Truncations map[TruncationReason][]int

		// TruncationsRASP provides details about truncations that occurred while
		// encoding address data for RASP execution.
		TruncationsRASP map[TruncationReason][]int
	}

	// Scope is the way to classify the different runs in the same context in order to have different metrics
	Scope string

	metricKey struct {
		scope     Scope
		component string
	}

	metricsStore struct {
		data  map[metricKey]time.Duration
		mutex sync.RWMutex
	}
)

const (
	DefaultScope Scope = "waf"
	RASPScope    Scope = "rasp"
)

const (
	wafEncodeTag     = "encode"
	wafRunTag        = "duration_ext"
	wafDurationTag   = "duration"
	wafDecodeTag     = "decode"
	wafTimeoutTag    = "timeouts"
	wafTruncationTag = "truncations"
)

func dot(parts ...string) string {
	return strings.Join(parts, ".")
}

// Metrics transform the stats returned by the WAF into a map of key value metrics with values in microseconds.
// ex. {"waf.encode": 100, "waf.duration_ext": 300, "waf.duration": 200, "rasp.encode": 100, "rasp.duration_ext": 300, "rasp.duration": 200}
func (stats Stats) Metrics() map[string]any {
	tags := make(map[string]any, len(stats.Timers)+len(stats.Truncations)+1)
	for k, v := range stats.Timers {
		tags[k] = float64(v.Nanoseconds()) / float64(time.Microsecond) // The metrics should be in microseconds
	}

	if stats.TimeoutCount > 0 {
		tags[dot(string(DefaultScope), wafTimeoutTag)] = stats.TimeoutCount
	}

	if stats.TimeoutRASPCount > 0 {
		tags[dot(string(RASPScope), wafTimeoutTag)] = stats.TimeoutRASPCount
	}

	for reason, list := range stats.Truncations {
		tags[dot(string(DefaultScope), wafTruncationTag, reason.String())] = list
	}

	for reason, list := range stats.TruncationsRASP {
		tags[dot(string(RASPScope), wafTruncationTag, reason.String())] = list
	}

	return tags
}

func (metrics *metricsStore) add(scope Scope, component string, duration time.Duration) {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	if metrics.data == nil {
		metrics.data = make(map[metricKey]time.Duration, 5)
	}

	metrics.data[metricKey{scope, component}] += duration
}

func (metrics *metricsStore) get(scope Scope, component string) time.Duration {
	metrics.mutex.RLock()
	defer metrics.mutex.RUnlock()
	return metrics.data[metricKey{scope, component}]
}

func (metrics *metricsStore) timers() map[string]time.Duration {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	if metrics.data == nil {
		return nil
	}

	timers := make(map[string]time.Duration, len(metrics.data))
	for k, v := range metrics.data {
		timers[dot(string(k.scope), k.component)] = v
	}
	return timers
}

// merge merges the current metrics with new ones
func (metrics *metricsStore) merge(scope Scope, other map[string]time.Duration) {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	if metrics.data == nil {
		metrics.data = make(map[metricKey]time.Duration, 5)
	}

	for component, val := range other {
		key := metricKey{scope, component}
		prev, ok := metrics.data[key]
		if !ok {
			prev = 0
		}
		metrics.data[key] = prev + val
	}
}
