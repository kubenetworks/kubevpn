// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

//go:generate msgp -o event_msgp.go -tests=false

package stacktrace

import (
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal"

	"github.com/tinylib/msgp/msgp"
)

var _ msgp.Marshaler = (*Event)(nil)

type EventCategory string

const (
	// ExceptionEvent is the event type for exception events
	ExceptionEvent EventCategory = "exception"
	// VulnerabilityEvent is the event type for vulnerability events
	VulnerabilityEvent EventCategory = "vulnerability"
	// ExploitEvent is the event type for exploit events
	ExploitEvent EventCategory = "exploit"
)

const SpanKey = "_dd.stack"

// Event is the toplevel structure to contain a stacktrace and the additional information needed to correlate it with other data
type Event struct {
	// Category is a well-known type of the event, not optional
	Category EventCategory `msg:"-"`
	// Type is a value event category specific, optional
	Type string `msg:"type,omitempty"`
	// Language is the language of the code that generated the event (set to "go" anyway here)
	Language string `msg:"language,omitempty"`
	// ID is the id of the event, optional for exceptions but mandatory for vulnerabilities and exploits to correlate with more data
	ID string `msg:"id,omitempty"`
	// Message is a generic message for the event
	Message string `msg:"message,omitempty"`
	// Frames is the stack trace of the event
	Frames StackTrace `msg:"frames"`
}

// NewEvent creates a new stacktrace event with the given category, type and message
func NewEvent(eventCat EventCategory, options ...Options) *Event {
	event := &Event{
		Category: eventCat,
		Language: "go",
		Frames:   SkipAndCapture(defaultCallerSkip),
	}

	for _, opt := range options {
		opt(event)
	}

	return event
}

// Options is a function type to set optional parameters for the event
type Options func(*Event)

// WithType sets the type of the event
func WithType(eventType string) Options {
	return func(event *Event) {
		event.Type = eventType
	}
}

// WithMessage sets the message of the event
func WithMessage(message string) Options {
	return func(event *Event) {
		event.Message = message
	}
}

// WithID sets the id of the event
func WithID(id string) Options {
	return func(event *Event) {
		event.ID = id
	}
}

// GetSpanValue returns the value to be set as a tag on a span for the given stacktrace events
func GetSpanValue(events ...*Event) any {
	if !Enabled() {
		return nil
	}

	groupByCategory := make(map[string][]*Event, 3)
	for _, event := range events {
		if _, ok := groupByCategory[string(event.Category)]; !ok {
			groupByCategory[string(event.Category)] = []*Event{event}
			continue
		}
		groupByCategory[string(event.Category)] = append(groupByCategory[string(event.Category)], event)
	}

	return internal.MetaStructValue{Value: groupByCategory}
}

// AddToSpan adds the event to the given span's root span as a tag if stacktrace collection is enabled
func AddToSpan(span ddtrace.Span, events ...*Event) {
	value := GetSpanValue(events...)
	type rooter interface {
		Root() ddtrace.Span
	}
	if lrs, ok := span.(rooter); ok {
		span = lrs.Root()
	}
	span.SetTag(SpanKey, value)
}
