// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package ext

const (
	// LogKeyTraceID is used by log integrations to correlate logs with a given trace.
	LogKeyTraceID = "dd.trace_id"
	// LogKeySpanID is used by log integrations to correlate logs with a given span.
	LogKeySpanID = "dd.span_id"
)
