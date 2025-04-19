// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// CIVisibilityEnabledEnvironmentVariable indicates if CI Visibility mode is enabled.
	// This environment variable should be set to "1" or "true" to enable CI Visibility mode, which activates tracing and other
	// features related to CI Visibility in the Datadog platform.
	CIVisibilityEnabledEnvironmentVariable = "DD_CIVISIBILITY_ENABLED"

	// CIVisibilityAgentlessEnabledEnvironmentVariable indicates if CI Visibility agentless mode is enabled.
	// This environment variable should be set to "1" or "true" to enable agentless mode for CI Visibility, where traces
	// are sent directly to Datadog without using a local agent.
	CIVisibilityAgentlessEnabledEnvironmentVariable = "DD_CIVISIBILITY_AGENTLESS_ENABLED"

	// CIVisibilityAgentlessURLEnvironmentVariable forces the agentless URL to a custom one.
	// This environment variable allows you to specify a custom URL for the agentless intake in CI Visibility mode.
	CIVisibilityAgentlessURLEnvironmentVariable = "DD_CIVISIBILITY_AGENTLESS_URL"

	// APIKeyEnvironmentVariable indicates the API key to be used for agentless intake.
	// This environment variable should be set to your Datadog API key, allowing the agentless mode to authenticate and
	// send data directly to the Datadog platform.
	APIKeyEnvironmentVariable = "DD_API_KEY"

	// CIVisibilityTestSessionNameEnvironmentVariable indicate the test session name to be used on CI Visibility payloads
	CIVisibilityTestSessionNameEnvironmentVariable = "DD_TEST_SESSION_NAME"

	// CIVisibilityFlakyRetryEnabledEnvironmentVariable kill-switch that allows to explicitly disable retries even if the remote setting is enabled.
	// This environment variable should be set to "0" or "false" to disable the flaky retry feature.
	CIVisibilityFlakyRetryEnabledEnvironmentVariable = "DD_CIVISIBILITY_FLAKY_RETRY_ENABLED"

	// CIVisibilityFlakyRetryCountEnvironmentVariable indicates the maximum number of retry attempts for a single test case.
	CIVisibilityFlakyRetryCountEnvironmentVariable = "DD_CIVISIBILITY_FLAKY_RETRY_COUNT"

	// CIVisibilityTotalFlakyRetryCountEnvironmentVariable indicates the maximum number of retry attempts for the entire session.
	CIVisibilityTotalFlakyRetryCountEnvironmentVariable = "DD_CIVISIBILITY_TOTAL_FLAKY_RETRY_COUNT"
)
