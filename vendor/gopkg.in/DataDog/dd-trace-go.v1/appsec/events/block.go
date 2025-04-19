// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

// Package events provides security event types that appsec can return in function calls it monitors when blocking them.
// It allows finer-grained integrations of appsec into your Go errors' management logic.
package events

import "errors"

var _ error = (*BlockingSecurityEvent)(nil)

// BlockingSecurityEvent is the error type returned by function calls blocked by appsec.
// Even though appsec takes care of responding automatically to the blocked requests, it
// is your duty to abort the request handlers that are calling functions blocked by appsec.
// For instance, if a gRPC handler performs a SQL query blocked by appsec, the SQL query
// function call gets blocked and aborted by returning an error of type SecurityBlockingEvent.
// This allows you to safely abort your request handlers, and to be able to leverage errors.As if
// necessary in your Go error management logic to be able to tell if the error is a blocking security
// event or not (eg. to avoid retrying an HTTP client request).
type BlockingSecurityEvent struct{}

func (*BlockingSecurityEvent) Error() string {
	return "request blocked by WAF"
}

// IsSecurityError returns true if the error is a security event.
func IsSecurityError(err error) bool {
	var secErr *BlockingSecurityEvent
	return errors.As(err, &secErr)
}
