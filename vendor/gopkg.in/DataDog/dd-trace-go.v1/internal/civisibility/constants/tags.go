// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// Origin is a tag used to indicate the origin of the data.
	// This tag helps in identifying the source of the trace data.
	Origin = "_dd.origin"

	// LogicalCPUCores is a tag used to indicate the number of logical cpu cores
	// This tag is used by the backend to perform calculations
	LogicalCPUCores = "_dd.host.vcpu_count"

	// CIAppTestOrigin defines the CIApp test origin value.
	// This constant is used to tag traces that originate from CIApp test executions.
	CIAppTestOrigin = "ciapp-test"

	// TestSessionIDTag defines the test session ID tag for the CI Visibility Protocol.
	// This constant is used to tag traces with the ID of the test session.
	TestSessionIDTag string = "test_session_id"

	// TestModuleIDTag defines the test module ID tag for the CI Visibility Protocol.
	// This constant is used to tag traces with the ID of the test module.
	TestModuleIDTag string = "test_module_id"

	// TestSuiteIDTag defines the test suite ID tag for the CI Visibility Protocol.
	// This constant is used to tag traces with the ID of the test suite.
	TestSuiteIDTag string = "test_suite_id"

	// ItrCorrelationIDTag defines the correlation ID for the intelligent test runner tag for the CI Visibility Protocol.
	// This constant is used to tag traces with the correlation ID for intelligent test runs.
	ItrCorrelationIDTag string = "itr_correlation_id"

	// UserProvidedTestServiceTag defines if the user provided the test service.
	UserProvidedTestServiceTag string = "_dd.test.is_user_provided_service"
)

// Coverage tags
const (
	// CodeCoverageEnabledTag defines if code coverage has been enabled.
	// This constant is used to tag traces to indicate whether code coverage measurement is enabled.
	CodeCoverageEnabledTag string = "test.code_coverage.enabled"

	// CodeCoveragePercentageOfTotalLines defines the percentage of total code coverage by a session.
	// This constant is used to tag traces with the percentage of code lines covered during the test session.
	CodeCoveragePercentageOfTotalLines string = "test.code_coverage.lines_pct"
)
