// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

//go:generate msgp -unexported -marshal=false -o=civisibility_tslv_msgp.go -tests=false

package tracer

import (
	"strconv"

	"github.com/tinylib/msgp/msgp"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/constants"
)

type (
	// ciTestCyclePayloadList implements msgp.Decodable on top of a slice of ciVisibilityPayloads.
	// This type is only used in tests.
	ciTestCyclePayloadList []*ciTestCyclePayload

	// ciVisibilityEvents is a slice of ciVisibilityEvent pointers.
	ciVisibilityEvents []*ciVisibilityEvent
)

// Ensure that ciVisibilityEvent and related types implement necessary interfaces.
var (
	_ ddtrace.Span   = (*ciVisibilityEvent)(nil)
	_ msgp.Encodable = (*ciVisibilityEvent)(nil)
	_ msgp.Decodable = (*ciVisibilityEvent)(nil)

	_ msgp.Encodable = (*ciVisibilityEvents)(nil)
	_ msgp.Decodable = (*ciVisibilityEvents)(nil)

	_ msgp.Encodable = (*ciTestCyclePayload)(nil)
	_ msgp.Decodable = (*ciTestCyclePayloadList)(nil)
)

// ciTestCyclePayload represents the payload for CI test cycles, including version, metadata, and events.
type ciTestCyclePayload struct {
	Version  int32                        `msg:"version"`  // Version of the payload format
	Metadata map[string]map[string]string `msg:"metadata"` // Metadata associated with the payload
	Events   msgp.Raw                     `msg:"events"`   // Encoded events data
}

// ciVisibilityEvent represents a CI visibility event, including type, version, and content.
// It implements the ddtrace.Span interface.
// According to the CI Visibility event specification it has the following format for tests:
//
//	{
//	   "type": "test",
//	   "version": 2,
//	   "content": {
//	       "type": "test",
//	       "trace_id": 123456,
//	       "span_id": 654321,
//	       "parent_id": 0,
//	       "test_session_id": 123456789,
//	       "test_module_id": 234567890,
//	       "test_suite_id": 123123123,
//	       "name": "...",
//	       "resource": "...",
//	       "error": 0,
//	       "meta": {
//	           ...
//	       },
//	       "metrics": {
//				  ...
//	       },
//	       "start": 1654698415668011500,
//	       "duration": 796143,
//	       "service": "..."
//	   }
//	}
//
// For test suites:
//
//	{
//	   "type": "test_suite_end",
//	   "version": 1,
//	   "content": {
//	       "type": "test_suite_end",
//	       "test_module_id": 234567890,
//	       "test_session_id": 123456789,
//	       "test_suite_id": 123123123,
//	       "name": "...",
//	       "resource": "...",
//	       "error": 0,
//		   "meta": {
//		    	...
//		   },
//		   "metrics": {
//				...
//		   },
//	       "start": 1654698415668011500,
//	       "duration": 796143,
//	       "service": "..."
//	   }
//	}
//
// For test modules:
//
//	{
//	 "type": "test_module_end",
//	 "version": 1,
//	 "content": {
//	     "type": "test_module_end",
//	     "test_session_id": 123456789,
//	     "test_module_id": 234567890,
//	     "error": 0,
//	     "name": "...",
//	     "resource": "...",
//		 "meta": {
//		    ...
//		 },
//		 "metrics": {
//			...
//		 },
//	     "start": 1654698415668011500,
//	     "duration": 796143,
//	     "service": "..."
//	 }
//	}
//
// For test sessions:
//
//	{
//	   "type": "test_session_end",
//	   "version": 1,
//	   "content": {
//	       "type": "test_session_end",
//	       "test_session_id": 123456789,
//	       "name": "...",
//	       "resource": "...",
//	       "error": 0,
//			"meta": {
//		    	...
//		 	},
//		 	"metrics": {
//				...
//		 	},
//	       "start": 1654698415668011500,
//	       "duration": 796143,
//	       "service": "..."
//	   }
//	}
//
// A complete specification for the meta and metrics maps for each type can be found at: https://github.com/DataDog/datadog-ci-spec/tree/main/spec/citest
type ciVisibilityEvent struct {
	Type    string   `msg:"type"`    // Type of the CI visibility event
	Version int32    `msg:"version"` // Version of the event type
	Content tslvSpan `msg:"content"` // Content of the event

	span *span `msg:"-"` // Associated span (not marshaled)
}

// SetTag sets a tag on the event's span and updates the content metadata and metrics.
//
// Parameters:
//
//	key - The tag key.
//	value - The tag value.
func (e *ciVisibilityEvent) SetTag(key string, value interface{}) {
	e.span.SetTag(key, value)
	e.Content.Meta = e.span.Meta
	e.Content.Metrics = e.span.Metrics
}

// SetOperationName sets the operation name of the event's span and updates the content name.
//
// Parameters:
//
//	operationName - The new operation name.
func (e *ciVisibilityEvent) SetOperationName(operationName string) {
	e.span.SetOperationName(operationName)
	e.Content.Name = e.span.Name
}

// BaggageItem retrieves the baggage item associated with the given key from the event's span.
//
// Parameters:
//
//	key - The baggage item key.
//
// Returns:
//
//	The baggage item value.
func (e *ciVisibilityEvent) BaggageItem(key string) string {
	return e.span.BaggageItem(key)
}

// SetBaggageItem sets a baggage item on the event's span.
//
// Parameters:
//
//	key - The baggage item key.
//	val - The baggage item value.
func (e *ciVisibilityEvent) SetBaggageItem(key, val string) {
	e.span.SetBaggageItem(key, val)
}

// Finish completes the event's span with optional finish options.
//
// Parameters:
//
//	opts - Optional finish options.
func (e *ciVisibilityEvent) Finish(opts ...ddtrace.FinishOption) {
	e.span.Finish(opts...)
}

// Context returns the span context of the event's span.
//
// Returns:
//
//	The span context.
func (e *ciVisibilityEvent) Context() ddtrace.SpanContext {
	return e.span.Context()
}

// tslvSpan represents the detailed information of a span for CI visibility.
type tslvSpan struct {
	SessionID     uint64             `msg:"test_session_id,omitempty"`    // identifier of this session
	ModuleID      uint64             `msg:"test_module_id,omitempty"`     // identifier of this module
	SuiteID       uint64             `msg:"test_suite_id,omitempty"`      // identifier of this suite
	CorrelationID string             `msg:"itr_correlation_id,omitempty"` // Correlation Id for Intelligent Test Runner transactions
	Name          string             `msg:"name"`                         // operation name
	Service       string             `msg:"service"`                      // service name (i.e. "grpc.server", "http.request")
	Resource      string             `msg:"resource"`                     // resource name (i.e. "/user?id=123", "SELECT * FROM users")
	Type          string             `msg:"type"`                         // protocol associated with the span (i.e. "web", "db", "cache")
	Start         int64              `msg:"start"`                        // span start time expressed in nanoseconds since epoch
	Duration      int64              `msg:"duration"`                     // duration of the span expressed in nanoseconds
	SpanID        uint64             `msg:"span_id,omitempty"`            // identifier of this span
	TraceID       uint64             `msg:"trace_id,omitempty"`           // lower 64-bits of the root span identifier
	ParentID      uint64             `msg:"parent_id,omitempty"`          // identifier of the span's direct parent
	Error         int32              `msg:"error"`                        // error status of the span; 0 means no errors
	Meta          map[string]string  `msg:"meta,omitempty"`               // arbitrary map of metadata
	Metrics       map[string]float64 `msg:"metrics,omitempty"`            // arbitrary map of numeric metrics
}

// getCiVisibilityEvent creates a ciVisibilityEvent from a span based on the span type.
//
// Parameters:
//
//	span - The span to convert into a ciVisibilityEvent.
//
// Returns:
//
//	A pointer to the created ciVisibilityEvent.
func getCiVisibilityEvent(span *span) *ciVisibilityEvent {
	switch span.Type {
	case constants.SpanTypeTest:
		return createTestEventFromSpan(span)
	case constants.SpanTypeTestSuite:
		return createTestSuiteEventFromSpan(span)
	case constants.SpanTypeTestModule:
		return createTestModuleEventFromSpan(span)
	case constants.SpanTypeTestSession:
		return createTestSessionEventFromSpan(span)
	default:
		return createSpanEventFromSpan(span)
	}
}

// createTestEventFromSpan creates a ciVisibilityEvent of type Test from a span.
//
// Parameters:
//
//	span - The span to convert.
//
// Returns:
//
//	A pointer to the created ciVisibilityEvent.
func createTestEventFromSpan(span *span) *ciVisibilityEvent {
	tSpan := createTslvSpan(span)
	tSpan.ParentID = 0
	tSpan.SessionID = getAndRemoveMetaToUInt64(span, constants.TestSessionIDTag)
	tSpan.ModuleID = getAndRemoveMetaToUInt64(span, constants.TestModuleIDTag)
	tSpan.SuiteID = getAndRemoveMetaToUInt64(span, constants.TestSuiteIDTag)
	tSpan.CorrelationID = getAndRemoveMeta(span, constants.ItrCorrelationIDTag)
	tSpan.SpanID = span.SpanID
	tSpan.TraceID = span.TraceID
	return &ciVisibilityEvent{
		span:    span,
		Type:    constants.SpanTypeTest,
		Version: 2,
		Content: tSpan,
	}
}

// createTestSuiteEventFromSpan creates a ciVisibilityEvent of type TestSuite from a span.
//
// Parameters:
//
//	span - The span to convert.
//
// Returns:
//
//	A pointer to the created ciVisibilityEvent.
func createTestSuiteEventFromSpan(span *span) *ciVisibilityEvent {
	tSpan := createTslvSpan(span)
	tSpan.ParentID = 0
	tSpan.SessionID = getAndRemoveMetaToUInt64(span, constants.TestSessionIDTag)
	tSpan.ModuleID = getAndRemoveMetaToUInt64(span, constants.TestModuleIDTag)
	tSpan.SuiteID = getAndRemoveMetaToUInt64(span, constants.TestSuiteIDTag)
	return &ciVisibilityEvent{
		span:    span,
		Type:    constants.SpanTypeTestSuite,
		Version: 1,
		Content: tSpan,
	}
}

// createTestModuleEventFromSpan creates a ciVisibilityEvent of type TestModule from a span.
//
// Parameters:
//
//	span - The span to convert.
//
// Returns:
//
//	A pointer to the created ciVisibilityEvent.
func createTestModuleEventFromSpan(span *span) *ciVisibilityEvent {
	tSpan := createTslvSpan(span)
	tSpan.ParentID = 0
	tSpan.SessionID = getAndRemoveMetaToUInt64(span, constants.TestSessionIDTag)
	tSpan.ModuleID = getAndRemoveMetaToUInt64(span, constants.TestModuleIDTag)
	return &ciVisibilityEvent{
		span:    span,
		Type:    constants.SpanTypeTestModule,
		Version: 1,
		Content: tSpan,
	}
}

// createTestSessionEventFromSpan creates a ciVisibilityEvent of type TestSession from a span.
//
// Parameters:
//
//	span - The span to convert.
//
// Returns:
//
//	A pointer to the created ciVisibilityEvent.
func createTestSessionEventFromSpan(span *span) *ciVisibilityEvent {
	tSpan := createTslvSpan(span)
	tSpan.ParentID = 0
	tSpan.SessionID = getAndRemoveMetaToUInt64(span, constants.TestSessionIDTag)
	return &ciVisibilityEvent{
		span:    span,
		Type:    constants.SpanTypeTestSession,
		Version: 1,
		Content: tSpan,
	}
}

// createSpanEventFromSpan creates a ciVisibilityEvent of type Span from a span.
//
// Parameters:
//
//	span - The span to convert.
//
// Returns:
//
//	A pointer to the created ciVisibilityEvent.
func createSpanEventFromSpan(span *span) *ciVisibilityEvent {
	tSpan := createTslvSpan(span)
	tSpan.SpanID = span.SpanID
	tSpan.TraceID = span.TraceID
	return &ciVisibilityEvent{
		span:    span,
		Type:    constants.SpanTypeSpan,
		Version: 1,
		Content: tSpan,
	}
}

// createTslvSpan creates a tslvSpan from a span.
//
// Parameters:
//
//	span - The span to convert.
//
// Returns:
//
//	The created tslvSpan.
func createTslvSpan(span *span) tslvSpan {
	return tslvSpan{
		Name:     span.Name,
		Service:  span.Service,
		Resource: span.Resource,
		Type:     span.Type,
		Start:    span.Start,
		Duration: span.Duration,
		ParentID: span.ParentID,
		Error:    span.Error,
		Meta:     span.Meta,
		Metrics:  span.Metrics,
	}
}

// getAndRemoveMeta retrieves a metadata value from a span and removes it from the span's metadata and metrics.
//
// Parameters:
//
//	span - The span to modify.
//	key - The metadata key to retrieve and remove.
//
// Returns:
//
//	The retrieved metadata value.
func getAndRemoveMeta(span *span, key string) string {
	span.Lock()
	defer span.Unlock()
	if span.Meta == nil {
		span.Meta = make(map[string]string, 1)
	}

	if v, ok := span.Meta[key]; ok {
		delete(span.Meta, key)
		delete(span.Metrics, key)
		return v
	}

	return ""
}

// getAndRemoveMetaToUInt64 retrieves a metadata value from a span, removes it, and converts it to a uint64.
//
// Parameters:
//
//	span - The span to modify.
//	key - The metadata key to retrieve and convert.
//
// Returns:
//
//	The retrieved and converted metadata value as a uint64.
func getAndRemoveMetaToUInt64(span *span, key string) uint64 {
	strValue := getAndRemoveMeta(span, key)
	i, err := strconv.ParseUint(strValue, 10, 64)
	if err != nil {
		return 0
	}
	return i
}
