// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package usersec

import (
	"context"
	"sync"

	"gopkg.in/DataDog/dd-trace-go.v1/appsec/events"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

const errorLog = `
appsec: user login monitoring ignored: could not find the http handler instrumentation metadata in the request context:
	the request handler is not being monitored by a middleware function or the provided context is not the expected request context.
	If the user has been blocked using remote rules, blocking will still be enforced but it will not be reported.
`

var errorLogOnce sync.Once

type (
	// UserEventType is the type of user event, such as a successful login or a failed login or any other authenticated request.
	UserEventType int

	// UserLoginOperation type representing a call to appsec.SetUser(). It gets both created and destroyed in a single
	// call to ExecuteUserIDOperation
	UserLoginOperation struct {
		dyngo.Operation
		EventType UserEventType
	}
	// UserLoginOperationArgs is the user ID operation arguments.
	UserLoginOperationArgs struct {
	}

	// UserLoginOperationRes is the user ID operation results.
	UserLoginOperationRes struct {
		UserID    string
		SessionID string
	}
)

const (
	// UserLoginSuccess is the event type for a successful user login, when a new session or JWT is created.
	UserLoginSuccess UserEventType = iota
	// UserLoginFailure is the event type for a failed user login, when the user ID is not found or the password is incorrect.
	UserLoginFailure
	// UserSet is the event type for a user ID operation that is not a login, such as any authenticated request made by the user.
	UserSet
)

func StartUserLoginOperation(ctx context.Context, eventType UserEventType, args UserLoginOperationArgs) (*UserLoginOperation, *error) {
	parent, ok := dyngo.FromContext(ctx)
	if !ok { // Nothing will be reported in this case, but we can still block so we don't return
		errorLogOnce.Do(func() { log.Error(errorLog) })
	}

	op := &UserLoginOperation{Operation: dyngo.NewOperation(parent), EventType: eventType}
	var err error
	dyngo.OnData(op, func(e *events.BlockingSecurityEvent) { err = e })
	dyngo.StartOperation(op, args)
	return op, &err
}

func (op *UserLoginOperation) Finish(args UserLoginOperationRes) {
	dyngo.FinishOperation(op, args)
}

func (UserLoginOperationArgs) IsArgOf(*UserLoginOperation)   {}
func (UserLoginOperationRes) IsResultOf(*UserLoginOperation) {}
