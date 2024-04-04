// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package timer

import (
	"time"
)

// clock is a simple cache for time.Now() to hopefully avoid some expensive calls to REALTIME part of time.Now()
type clock struct {
	lastRequest time.Time
}

func newTimeCache() clock {
	return clock{
		lastRequest: time.Now(),
	}
}

func (ct *clock) now() time.Time {
	// If the diff is greater than ~2^32 then the monotonic clock has wrapped around
	// and time.Since will do a call to time.Now() for us.
	ct.lastRequest = ct.lastRequest.Add(time.Since(ct.lastRequest))
	return ct.lastRequest
}
