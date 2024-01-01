// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package internal

type contextKey struct{}

// ActiveSpanKey is used to set tracer context on a context.Context objects with a unique key
var ActiveSpanKey = contextKey{}
