// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

// Package telemetry implements a client for sending telemetry information to
// Datadog regarding usage of an APM library such as tracing or profiling.
package telemetry

import (
	"fmt"
	"math"
	"sort"
	"strings"
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

// Sanitize ensures the configuration values are valid and compatible.
// It removes NaN and Inf values and converts string slices and maps into comma-separated strings.
func Sanitize(c Configuration) Configuration {
	switch val := c.Value.(type) {
	case float64:
		if math.IsNaN(val) || math.IsInf(val, 0) {
			// Those values cause marshalling errors.
			// https://github.com/golang/go/issues/59627
			c.Value = nil
		}
	case []string:
		// The telemetry API only supports primitive types.
		c.Value = strings.Join(val, ",")
	case map[string]interface{}:
		// The telemetry API only supports primitive types.
		// Sort the keys to ensure the order is deterministic.
		// This is technically not required but makes testing easier + it's not in a hot path.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, k := range keys {
			if sb.Len() > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(k)
			sb.WriteString(":")
			sb.WriteString(fmt.Sprint(val[k]))
		}
		c.Value = sb.String()
	}
	return c
}
