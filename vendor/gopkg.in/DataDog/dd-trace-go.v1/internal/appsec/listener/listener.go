// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package listener provides functions and types used to listen to AppSec
// instrumentation events produced by code usintrumented using the functions and
// types found in gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/emitter.
package listener

import waf "github.com/DataDog/go-libddwaf/v2"

// ContextKey is used as a key to store operations in the request's context (gRPC/HTTP)
type ContextKey struct{}

// AddressSet is a set of WAF addresses.
type AddressSet map[string]struct{}

// FilterAddressSet filters the supplied `supported` address set to only include
// entries referenced by the supplied waf.Handle.
func FilterAddressSet(supported AddressSet, handle *waf.Handle) AddressSet {
	result := make(AddressSet, len(supported))

	for _, addr := range handle.Addresses() {
		if _, found := supported[addr]; found {
			result[addr] = struct{}{}
		}
	}

	return result
}
