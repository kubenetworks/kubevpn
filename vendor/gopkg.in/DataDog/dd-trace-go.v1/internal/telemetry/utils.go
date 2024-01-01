// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

// Package telemetry implements a client for sending telemetry information to
// Datadog regarding usage of an APM library such as tracing or profiling.
package telemetry

import (
	"testing"
)

// MockGlobalClient replaces the global telemetry client with a custom
// implementation of TelemetryClient. It returns a function that can be deferred
// to reset the global telemetry client to its previous value.
func MockGlobalClient(client Client) func() {
	globalClient.Lock()
	defer globalClient.Unlock()
	oldClient := GlobalClient
	GlobalClient = client
	return func() {
		globalClient.Lock()
		defer globalClient.Unlock()
		GlobalClient = oldClient
	}
}

// Check is a testing utility to assert that a target key in config contains the expected value
func Check(t *testing.T, configuration []Configuration, key string, expected interface{}) {
	for _, kv := range configuration {
		if kv.Name == key {
			if kv.Value != expected {
				t.Errorf("configuration %s: wanted %v, got %v", key, expected, kv.Value)
			}
			return
		}
	}
	t.Errorf("missing configuration %s", key)
}

// SetAgentlessEndpoint is used for testing purposes to replace the real agentless
// endpoint with a custom one
func SetAgentlessEndpoint(endpoint string) string {
	agentlessEndpointLock.Lock()
	defer agentlessEndpointLock.Unlock()
	prev := agentlessURL
	agentlessURL = endpoint
	return prev
}
