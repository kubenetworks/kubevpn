// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// SpanTypeTest marks a span as a test execution.
	// This constant is used to indicate that the span represents a test execution.
	SpanTypeTest = "test"

	// SpanTypeTestSuite marks a span as a test suite.
	// This constant is used to indicate that the span represents the end of a test suite.
	SpanTypeTestSuite = "test_suite_end"

	// SpanTypeTestModule marks a span as a test module.
	// This constant is used to indicate that the span represents the end of a test module.
	SpanTypeTestModule = "test_module_end"

	// SpanTypeTestSession marks a span as a test session.
	// This constant is used to indicate that the span represents the end of a test session.
	SpanTypeTestSession = "test_session_end"

	// SpanTypeSpan marks a span as a span event.
	// This constant is used to indicate that the span represents a general span event.
	SpanTypeSpan = "span"
)
