// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package telemetry

// TestingFramework is a type for testing frameworks
type TestingFramework string

const (
	GoTestingFramework TestingFramework = "test_framework:testing"
	UnknownFramework   TestingFramework = "test_framework:unknown"
)

// TestingEventType is a type for testing event types
type TestingEventType []string

var (
	TestEventType    TestingEventType = []string{"event_type:test"}
	SuiteEventType   TestingEventType = []string{"event_type:suite"}
	ModuleEventType  TestingEventType = []string{"event_type:module"}
	SessionEventType TestingEventType = []string{"event_type:session"}

	UnsupportedCiEventType TestingEventType = []string{"is_unsupported_ci"}
	HasCodeOwnerEventType  TestingEventType = []string{"has_codeowner"}
	IsNewEventType         TestingEventType = []string{"is_new:true"}
	IsRetryEventType       TestingEventType = []string{"is_retry:true"}
	EfdAbortSlowEventType  TestingEventType = []string{"early_flake_detection_abort_reason:slow"}
	IsBenchmarkEventType   TestingEventType = []string{"is_benchmark"}
)

// CoverageLibraryType is a type for coverage library types
type CoverageLibraryType string

const (
	DefaultCoverageLibraryType CoverageLibraryType = "library:default"
	UnknownCoverageLibraryType CoverageLibraryType = "library:unknown"
)

// EndpointType is a type for endpoint types
type EndpointType string

const (
	TestCycleEndpointType    EndpointType = "endpoint:test_cycle"
	CodeCoverageEndpointType EndpointType = "endpoint:code_coverage"
)

// ErrorType is a type for error types
type ErrorType []string

var (
	TimeoutErrorType       ErrorType = []string{"error_type:timeout"}
	NetworkErrorType       ErrorType = []string{"error_type:network"}
	StatusCodeErrorType    ErrorType = []string{"error_type:status_code"}
	StatusCode4xxErrorType ErrorType = []string{"error_type:status_code_4xx_response"}
	StatusCode5xxErrorType ErrorType = []string{"error_type:status_code_5xx_response"}
	StatusCode400ErrorType ErrorType = []string{"error_type:status_code_4xx_response", "status_code:400"}
	StatusCode401ErrorType ErrorType = []string{"error_type:status_code_4xx_response", "status_code:401"}
	StatusCode403ErrorType ErrorType = []string{"error_type:status_code_4xx_response", "status_code:403"}
	StatusCode404ErrorType ErrorType = []string{"error_type:status_code_4xx_response", "status_code:404"}
	StatusCode408ErrorType ErrorType = []string{"error_type:status_code_4xx_response", "status_code:408"}
	StatusCode429ErrorType ErrorType = []string{"error_type:status_code_4xx_response", "status_code:429"}
)

// CommandType is a type for commands types
type CommandType string

const (
	NotSpecifiedCommandsType    CommandType = ""
	GetRepositoryCommandsType   CommandType = "command:get_repository"
	GetBranchCommandsType       CommandType = "command:get_branch"
	GetRemoteCommandsType       CommandType = "command:get_remote"
	GetHeadCommandsType         CommandType = "command:get_head"
	CheckShallowCommandsType    CommandType = "command:check_shallow"
	UnshallowCommandsType       CommandType = "command:unshallow"
	GetLocalCommitsCommandsType CommandType = "command:get_local_commits"
	GetObjectsCommandsType      CommandType = "command:get_objects"
	PackObjectsCommandsType     CommandType = "command:pack_objects"
)

// CommandExitCodeType is a type for command exit codes
type CommandExitCodeType string

const (
	MissingCommandExitCode  CommandExitCodeType = "exit_code:missing"
	UnknownCommandExitCode  CommandExitCodeType = "exit_code:unknown"
	ECMinus1CommandExitCode CommandExitCodeType = "exit_code:-1"
	EC1CommandExitCode      CommandExitCodeType = "exit_code:1"
	EC2CommandExitCode      CommandExitCodeType = "exit_code:2"
	EC127CommandExitCode    CommandExitCodeType = "exit_code:127"
	EC128CommandExitCode    CommandExitCodeType = "exit_code:128"
	EC129CommandExitCode    CommandExitCodeType = "exit_code:129"
)

// RequestCompressedType is a type for request compressed types
type RequestCompressedType string

const (
	UncompressedRequestCompressedType RequestCompressedType = ""
	CompressedRequestCompressedType   RequestCompressedType = "rq_compressed:true"
)

// ResponseCompressedType is a type for response compressed types
type ResponseCompressedType string

const (
	UncompressedResponseCompressedType ResponseCompressedType = ""
	CompressedResponseCompressedType   ResponseCompressedType = "rs_compressed:true"
)

// SettingsResponseType is a type for settings response types
type SettingsResponseType []string

var (
	CoverageEnabledSettingsResponseType         SettingsResponseType = []string{"coverage_enabled"}
	ItrSkipEnabledSettingsResponseType          SettingsResponseType = []string{"itrskip_enabled"}
	EfdEnabledSettingsResponseType              SettingsResponseType = []string{"early_flake_detection_enabled:true"}
	FlakyTestRetriesEnabledSettingsResponseType SettingsResponseType = []string{"flaky_test_retries_enabled:true"}
)

// removeEmptyStrings removes empty string values inside an array or use the same if not empty string is found.
func removeEmptyStrings(s []string) []string {
	var r []string
	hasSpace := false
	for i, str := range s {
		if str == "" && r == nil {
			if i > 0 {
				r = s[:i]
			}
			hasSpace = true
			continue
		}
		if hasSpace {
			r = append(r, str)
		}
	}

	if r == nil {
		r = s
	}

	return r
}
