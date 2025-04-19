// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// OSPlatform indicates the operating system family (e.g., linux, windows, darwin).
	// This constant is used to tag traces with the operating system family on which the tests are running.
	OSPlatform = "os.platform"

	// OSVersion indicates the version of the operating system.
	// This constant is used to tag traces with the specific version of the operating system on which the tests are running.
	OSVersion = "os.version"

	// OSArchitecture indicates the architecture this SDK is built for (e.g., amd64, 386, arm).
	// This constant is used to tag traces with the architecture of the operating system for which the tests are built.
	// Note: This could be 32-bit on a 64-bit system.
	OSArchitecture = "os.architecture"
)
