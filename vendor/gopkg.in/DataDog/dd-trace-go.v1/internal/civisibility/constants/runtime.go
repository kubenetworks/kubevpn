// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// RuntimeName indicates the name of the runtime compiler.
	// This constant is used to tag traces with the name of the runtime compiler being used (e.g., Go, JVM).
	RuntimeName = "runtime.name"

	// RuntimeVersion indicates the version of the runtime compiler.
	// This constant is used to tag traces with the specific version of the runtime compiler being used.
	RuntimeVersion = "runtime.version"
)
