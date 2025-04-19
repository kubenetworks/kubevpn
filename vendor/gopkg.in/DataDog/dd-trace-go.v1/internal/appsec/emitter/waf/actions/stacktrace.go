// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package actions

import (
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/stacktrace"
)

func init() {
	registerActionHandler("generate_stack", NewStackTraceAction)
}

// StackTraceAction are actions that generate a stacktrace
type StackTraceAction struct {
	Event *stacktrace.Event
}

func (a *StackTraceAction) EmitData(op dyngo.Operation) { dyngo.EmitData(op, a) }

// NewStackTraceAction creates an action for the "stacktrace" action type
func NewStackTraceAction(params map[string]any) []Action {
	id, ok := params["stack_id"]
	if !ok {
		log.Debug("appsec: could not read stack_id parameter for generate_stack action")
		return nil
	}

	strID, ok := id.(string)
	if !ok {
		log.Debug("appsec: could not cast stacktrace ID to string")
		return nil
	}

	return []Action{
		&StackTraceAction{
			stacktrace.NewEvent(stacktrace.ExploitEvent, stacktrace.WithID(strID)),
		},
	}
}
