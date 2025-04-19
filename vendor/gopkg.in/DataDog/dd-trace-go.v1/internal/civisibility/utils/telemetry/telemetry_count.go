// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package telemetry

import (
	"gopkg.in/DataDog/dd-trace-go.v1/internal/telemetry"
)

func getTestingFramework(testingFramework string) TestingFramework {
	telemetryFramework := UnknownFramework
	if testingFramework == "golang.org/pkg/testing" {
		telemetryFramework = GoTestingFramework
	}
	return telemetryFramework
}

func GetErrorTypeFromStatusCode(statusCode int) ErrorType {
	switch statusCode {
	case 0:
		return NetworkErrorType
	case 400:
		return StatusCode400ErrorType
	case 401:
		return StatusCode401ErrorType
	case 403:
		return StatusCode403ErrorType
	case 404:
		return StatusCode404ErrorType
	case 408:
		return StatusCode408ErrorType
	case 429:
		return StatusCode429ErrorType
	default:
		if statusCode >= 500 && statusCode < 600 {
			return StatusCode5xxErrorType
		} else if statusCode >= 400 && statusCode < 500 {
			return StatusCode4xxErrorType
		} else {
			return StatusCodeErrorType
		}
	}
}

// EventCreated the number of events created by CI Visibility
func EventCreated(testingFramework string, eventType TestingEventType) {
	tags := []string{string(getTestingFramework(testingFramework))}
	tags = append(tags, eventType...)
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "event_created", 1.0, removeEmptyStrings(tags), true)
}

// EventFinished the number of events finished by CI Visibility
func EventFinished(testingFramework string, eventType TestingEventType) {
	tags := []string{string(getTestingFramework(testingFramework))}
	tags = append(tags, eventType...)
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "event_finished", 1.0, removeEmptyStrings(tags), true)
}

// CodeCoverageStarted the number of code coverage start calls by CI Visibility
func CodeCoverageStarted(testingFramework string, coverageLibraryType CoverageLibraryType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "code_coverage_started", 1.0, removeEmptyStrings([]string{
		string(getTestingFramework(testingFramework)),
		string(coverageLibraryType),
	}), true)
}

// CodeCoverageFinished the number of code coverage finished calls by CI Visibility
func CodeCoverageFinished(testingFramework string, coverageLibraryType CoverageLibraryType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "code_coverage_finished", 1.0, removeEmptyStrings([]string{
		string(getTestingFramework(testingFramework)),
		string(coverageLibraryType),
	}), true)
}

// EventsEnqueueForSerialization the number of events enqueued for serialization by CI Visibility
func EventsEnqueueForSerialization() {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "events_enqueued_for_serialization", 1.0, nil, true)
}

// EndpointPayloadRequests the number of requests sent to the endpoint, regardless of success, tagged by endpoint type
func EndpointPayloadRequests(endpointType EndpointType, requestCompressedType RequestCompressedType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "endpoint_payload.requests", 1.0, removeEmptyStrings([]string{
		string(endpointType),
		string(requestCompressedType),
	}), true)
}

// EndpointPayloadRequestsErrors the number of requests sent to the endpoint that errored, tagget by the error type and endpoint type and status code
func EndpointPayloadRequestsErrors(endpointType EndpointType, errorType ErrorType) {
	tags := []string{string(endpointType)}
	tags = append(tags, errorType...)
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "endpoint_payload.requests_errors", 1.0, removeEmptyStrings(tags), true)
}

// EndpointPayloadDropped the number of payloads dropped after all retries by CI Visibility
func EndpointPayloadDropped(endpointType EndpointType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "endpoint_payload.dropped", 1.0, removeEmptyStrings([]string{
		string(endpointType),
	}), true)
}

// GitCommand the number of git commands executed by CI Visibility
func GitCommand(commandType CommandType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git.command", 1.0, removeEmptyStrings([]string{
		string(commandType),
	}), true)
}

// GitCommandErrors the number of git command that errored by CI Visibility
func GitCommandErrors(commandType CommandType, exitCode CommandExitCodeType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git.command_errors", 1.0, removeEmptyStrings([]string{
		string(commandType),
		string(exitCode),
	}), true)
}

// GitRequestsSearchCommits the number of requests sent to the search commit endpoint, regardless of success.
func GitRequestsSearchCommits(requestCompressed RequestCompressedType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.search_commits", 1.0, removeEmptyStrings([]string{
		string(requestCompressed),
	}), true)
}

// GitRequestsSearchCommitsErrors the number of requests sent to the search commit endpoint that errored, tagged by the error type.
func GitRequestsSearchCommitsErrors(errorType ErrorType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.search_commits_errors", 1.0, removeEmptyStrings(errorType), true)
}

// GitRequestsObjectsPack the number of requests sent to the objects pack endpoint, tagged by the request compressed type.
func GitRequestsObjectsPack(requestCompressed RequestCompressedType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.objects_pack", 1.0, removeEmptyStrings([]string{
		string(requestCompressed),
	}), true)
}

// GitRequestsObjectsPackErrors the number of requests sent to the objects pack endpoint that errored, tagged by the error type.
func GitRequestsObjectsPackErrors(errorType ErrorType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.objects_pack_errors", 1.0, removeEmptyStrings(errorType), true)
}

// GitRequestsSettings the number of requests sent to the settings endpoint, tagged by the request compressed type.
func GitRequestsSettings(requestCompressed RequestCompressedType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.settings", 1.0, removeEmptyStrings([]string{
		string(requestCompressed),
	}), true)
}

// GitRequestsSettingsErrors the number of requests sent to the settings endpoint that errored, tagged by the error type.
func GitRequestsSettingsErrors(errorType ErrorType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.settings_errors", 1.0, removeEmptyStrings(errorType), true)
}

// GitRequestsSettingsResponse the number of settings responses received by CI Visibility, tagged by the settings response type.
func GitRequestsSettingsResponse(settingsResponseType SettingsResponseType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "git_requests.settings_response", 1.0, removeEmptyStrings(settingsResponseType), true)
}

// ITRSkippableTestsRequest the number of requests sent to the ITR skippable tests endpoint, tagged by the request compressed type.
func ITRSkippableTestsRequest(requestCompressed RequestCompressedType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "itr_skippable_tests.request", 1.0, removeEmptyStrings([]string{
		string(requestCompressed),
	}), true)
}

// ITRSkippableTestsRequestErrors the number of requests sent to the ITR skippable tests endpoint that errored, tagged by the error type.
func ITRSkippableTestsRequestErrors(errorType ErrorType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "itr_skippable_tests.request_errors", 1.0, removeEmptyStrings(errorType), true)
}

// ITRSkippableTestsResponseTests the number of tests received in the ITR skippable tests response by CI Visibility.
func ITRSkippableTestsResponseTests(value float64) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "itr_skippable_tests.response_tests", value, nil, true)
}

// ITRSkipped the number of ITR tests skipped by CI Visibility, tagged by the event type.
func ITRSkipped(eventType TestingEventType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "itr_skipped", 1.0, removeEmptyStrings(eventType), true)
}

// ITRUnskippable the number of ITR tests unskippable by CI Visibility, tagged by the event type.
func ITRUnskippable(eventType TestingEventType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "itr_unskippable", 1.0, removeEmptyStrings(eventType), true)
}

// ITRForcedRun the number of tests or test suites that would've been skipped by ITR but were forced to run because of their unskippable status by CI Visibility.
func ITRForcedRun(eventType TestingEventType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "itr_forced_run", 1.0, removeEmptyStrings(eventType), true)
}

// CodeCoverageIsEmpty the number of code coverage payloads that are empty by CI Visibility.
func CodeCoverageIsEmpty() {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "code_coverage.is_empty", 1.0, nil, true)
}

// CodeCoverageErrors the number of errors while processing code coverage by CI Visibility.
func CodeCoverageErrors() {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "code_coverage.errors", 1.0, nil, true)
}

// KnownTestsRequest the number of requests sent to the known tests endpoint, tagged by the request compressed type.
func KnownTestsRequest(requestCompressed RequestCompressedType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "known_tests.request", 1.0, removeEmptyStrings([]string{
		string(requestCompressed),
	}), true)
}

// KnownTestsRequestErrors the number of requests sent to the known tests endpoint that errored, tagged by the error type.
func KnownTestsRequestErrors(errorType ErrorType) {
	telemetry.GlobalClient.Count(telemetry.NamespaceCiVisibility, "known_tests.request_errors", 1.0, removeEmptyStrings(errorType), true)
}
