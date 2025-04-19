// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package telemetry

import "gopkg.in/DataDog/dd-trace-go.v1/internal/telemetry"

// EndpointPayloadBytes records the size in bytes of the serialized payload by CI Visibility.
func EndpointPayloadBytes(endpointType EndpointType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "endpoint_payload.bytes", value, removeEmptyStrings([]string{
		(string)(endpointType),
	}), true)
}

// EndpointPayloadRequestsMs records the time it takes to send the payload sent to the endpoint in ms by CI Visibility.
func EndpointPayloadRequestsMs(endpointType EndpointType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "endpoint_payload.requests_ms", value, removeEmptyStrings([]string{
		(string)(endpointType),
	}), true)
}

// EndpointPayloadEventsCount records the number of events in the payload sent to the endpoint by CI Visibility.
func EndpointPayloadEventsCount(endpointType EndpointType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "endpoint_payload.events_count", value, removeEmptyStrings([]string{
		(string)(endpointType),
	}), true)
}

// EndpointEventsSerializationMs records the time it takes to serialize the events in the payload sent to the endpoint in ms by CI Visibility.
func EndpointEventsSerializationMs(endpointType EndpointType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "endpoint_payload.events_serialization_ms", value, removeEmptyStrings([]string{
		(string)(endpointType),
	}), true)
}

// GitCommandMs records the time it takes to execute a git command in ms by CI Visibility.
func GitCommandMs(commandType CommandType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "git.command_ms", value, removeEmptyStrings([]string{
		(string)(commandType),
	}), true)
}

// GitRequestsSearchCommitsMs records the time it takes to get the response of the search commit quest in ms by CI Visibility.
func GitRequestsSearchCommitsMs(responseCompressedType ResponseCompressedType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "git_requests.search_commits_ms", value, removeEmptyStrings([]string{
		(string)(responseCompressedType),
	}), true)
}

// GitRequestsObjectsPackMs records the time it takes to get the response of the objects pack request in ms by CI Visibility.
func GitRequestsObjectsPackMs(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "git_requests.objects_pack_ms", value, nil, true)
}

// GitRequestsObjectsPackBytes records the sum of the sizes of the object pack files inside a single payload by CI Visibility
func GitRequestsObjectsPackBytes(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "git_requests.objects_pack_bytes", value, nil, true)
}

// GitRequestsObjectsPackFiles records the number of files sent in the object pack payload by CI Visibility.
func GitRequestsObjectsPackFiles(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "git_requests.objects_pack_files", value, nil, true)
}

// GitRequestsSettingsMs records the time it takes to get the response of the settings endpoint request in ms by CI Visibility.
func GitRequestsSettingsMs(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "git_requests.settings_ms", value, nil, true)
}

// ITRSkippableTestsRequestMs records the time it takes to get the response of the itr skippable tests endpoint request in ms by CI Visibility.
func ITRSkippableTestsRequestMs(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "itr_skippable_tests.request_ms", value, nil, true)
}

// ITRSkippableTestsResponseBytes records the number of bytes received by the endpoint. Tagged with a boolean flag set to true if response body is compressed.
func ITRSkippableTestsResponseBytes(responseCompressedType ResponseCompressedType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "itr_skippable_tests.response_bytes", value, removeEmptyStrings([]string{
		(string)(responseCompressedType),
	}), true)
}

// CodeCoverageFiles records the number of files in the code coverage report by CI Visibility.
func CodeCoverageFiles(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "code_coverage.files", value, nil, true)
}

// KnownTestsRequestMs records the time it takes to get the response of the known tests endpoint request in ms by CI Visibility.
func KnownTestsRequestMs(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "known_tests.request_ms", value, nil, true)
}

// KnownTestsResponseBytes records the number of bytes received by the endpoint. Tagged with a boolean flag set to true if response body is compressed.
func KnownTestsResponseBytes(responseCompressedType ResponseCompressedType, value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "known_tests.response_bytes", value, removeEmptyStrings([]string{
		(string)(responseCompressedType),
	}), true)
}

// KnownTestsResponseTests records the number of tests in the response of the known tests endpoint by CI Visibility.
func KnownTestsResponseTests(value float64) {
	telemetry.GlobalClient.Record(telemetry.NamespaceCiVisibility, telemetry.MetricKindDist, "known_tests.response_tests", value, nil, true)
}
