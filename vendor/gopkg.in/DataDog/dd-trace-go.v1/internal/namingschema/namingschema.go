// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

// Package namingschema provides functionality to create naming schemas used by integrations to set different
// service and span/operation names based on the value of the DD_TRACE_SPAN_ATTRIBUTE_SCHEMA environment variable.
// It also provides some already implemented schemas for common use cases (client-server, db, messaging, etc.).
//
// How to use this package:
// 1. Implement the VersionSupportSchema interface containing the correct name for each version.
// 2. Create a new Schema using the New function.
// 3. Call Schema.GetName to get the correct name based on the user configuration.
package namingschema

import (
	"strings"
	"sync"
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
	sv   Version
	svMu sync.RWMutex

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
	svMu.RLock()
	defer svMu.RUnlock()
	return sv
}

// SetVersion sets the global naming schema version used for this application.
func SetVersion(v Version) {
	svMu.Lock()
	defer svMu.Unlock()
	sv = v
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

// VersionSupportSchema is an interface that ensures all the available naming schema versions are implemented by the caller.
type VersionSupportSchema interface {
	V0() string
	V1() string
}

// Schema allows to select the proper name to use based on the given VersionSupportSchema.
type Schema struct {
	selectedVersion Version
	vSchema         VersionSupportSchema
}

// New initializes a new Schema.
func New(vSchema VersionSupportSchema) *Schema {
	return &Schema{
		selectedVersion: GetVersion(),
		vSchema:         vSchema,
	}
}

// GetName returns the proper name for this Schema for the user selected version.
func (s *Schema) GetName() string {
	switch s.selectedVersion {
	case SchemaV1:
		return s.vSchema.V1()
	default:
		return s.vSchema.V0()
	}
}
