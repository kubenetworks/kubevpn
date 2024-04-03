// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

// Package namingschema allows to use the naming schema from the integrations to set different
// service and span/operation names based on the value of the DD_TRACE_SPAN_ATTRIBUTE_SCHEMA environment variable.
package namingschema

import (
	"strings"
	"sync"
	"sync/atomic"
)

// Version represents the available naming schema versions.
type Version int

const (
	// SchemaV0 represents naming schema v0.
	SchemaV0 Version = iota
	// SchemaV1 represents naming schema v1.
	SchemaV1
)

const (
	defaultSchemaVersion = SchemaV0
)

var (
	sv int32

	useGlobalServiceName   bool
	useGlobalServiceNameMu sync.RWMutex
)

// ParseVersion attempts to parse the version string.
func ParseVersion(v string) (Version, bool) {
	switch strings.ToLower(v) {
	case "", "v0":
		return SchemaV0, true
	case "v1":
		return SchemaV1, true
	default:
		return SchemaV0, false
	}
}

// GetVersion returns the global naming schema version used for this application.
func GetVersion() Version {
	return Version(atomic.LoadInt32(&sv))
}

// SetVersion sets the global naming schema version used for this application.
func SetVersion(v Version) {
	atomic.StoreInt32(&sv, int32(v))
}

// SetDefaultVersion sets the default global naming schema version.
func SetDefaultVersion() Version {
	SetVersion(defaultSchemaVersion)
	return defaultSchemaVersion
}

// UseGlobalServiceName returns the value of the useGlobalServiceName setting for this application.
func UseGlobalServiceName() bool {
	useGlobalServiceNameMu.RLock()
	defer useGlobalServiceNameMu.RUnlock()
	return useGlobalServiceName
}

// SetUseGlobalServiceName sets the value of the useGlobalServiceName setting used for this application.
func SetUseGlobalServiceName(v bool) {
	useGlobalServiceNameMu.Lock()
	defer useGlobalServiceNameMu.Unlock()
	useGlobalServiceName = v
}
