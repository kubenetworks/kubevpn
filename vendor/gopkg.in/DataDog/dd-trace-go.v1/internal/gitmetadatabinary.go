// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

//go:build go1.18
// +build go1.18

package internal

import (
	"runtime/debug"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// getTagsFromBinary extracts git metadata from binary metadata
func getTagsFromBinary() map[string]string {
	res := make(map[string]string)
	info, ok := debug.ReadBuildInfo()
	if !ok {
		log.Debug("ReadBuildInfo failed, skip source code metadata extracting")
		return res
	}
	goPath := info.Path
	var vcs, commitSha string
	for _, s := range info.Settings {
		if s.Key == "vcs" {
			vcs = s.Value
		} else if s.Key == "vcs.revision" {
			commitSha = s.Value
		}
	}
	if vcs != "git" {
		log.Debug("Unknown VCS: '%s', skip source code metadata extracting", vcs)
		return res
	}
	res[TagCommitSha] = commitSha
	res[TagGoPath] = goPath
	return res
}
