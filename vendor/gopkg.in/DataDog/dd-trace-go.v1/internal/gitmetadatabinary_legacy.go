// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

//go:build !go1.18
// +build !go1.18

package internal

import (
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// getTagsFromBinary extracts git metadata from binary metadata
func getTagsFromBinary() map[string]string {
	log.Warn("go version below 1.18, BuildInfo has no vcs info, skip source code metadata extracting")
	return make(map[string]string)
}
