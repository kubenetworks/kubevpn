// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package actions

import (
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

type (
	// Action is a generic interface that represents any WAF action
	Action interface {
		EmitData(op dyngo.Operation)
	}
)

type actionHandler func(map[string]any) []Action

// actionHandlers is a map of action types to their respective handler functions
// It is populated by the init functions of the actions packages
var actionHandlers = map[string]actionHandler{}

func registerActionHandler(aType string, handler actionHandler) {
	if _, ok := actionHandlers[aType]; ok {
		log.Warn("appsec: action type `%s` already registered", aType)
		return
	}
	actionHandlers[aType] = handler
}

// SendActionEvents sends the relevant actions to the operation's data listener.
// It returns true if at least one of those actions require interrupting the request handler
// When SDKError is not nil, this error is sent to the op with EmitData so that the invoked SDK can return it
func SendActionEvents(op dyngo.Operation, actions map[string]any) {
	for aType, params := range actions {
		log.Debug("appsec: processing %s action with params %v", aType, params)
		params, ok := params.(map[string]any)
		if !ok {
			log.Debug("appsec: could not cast action params to map[string]any from %T", params)
			continue
		}

		actionHandler, ok := actionHandlers[aType]
		if !ok {
			log.Debug("appsec: unknown action type `%s`", aType)
			continue
		}

		for _, a := range actionHandler(params) {
			a.EmitData(op)
		}
	}
}
