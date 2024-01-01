// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package internal

import (
	"net/url"
	"os"
	"sync"
)

const (
	// EnvGitMetadataEnabledFlag specifies the environment variable name for enable/disable
	EnvGitMetadataEnabledFlag = "DD_TRACE_GIT_METADATA_ENABLED"
	// EnvGitRepositoryURL specifies the environment variable name for git repository URL
	EnvGitRepositoryURL = "DD_GIT_REPOSITORY_URL"
	// EnvGitCommitSha specifies the environment variable name git commit sha
	EnvGitCommitSha = "DD_GIT_COMMIT_SHA"
	// EnvDDTags specifies the environment variable name global tags
	EnvDDTags = "DD_TAGS"

	// TagRepositoryURL specifies the tag name for git repository URL
	TagRepositoryURL = "git.repository_url"
	// TagCommitSha specifies the tag name for git commit sha
	TagCommitSha = "git.commit.sha"
	// TagGoPath specifies the tag name for go module path
	TagGoPath = "go_path"

	// TraceTagRepositoryURL specifies the trace tag name for git repository URL
	TraceTagRepositoryURL = "_dd.git.repository_url"
	// TraceTagCommitSha specifies the trace tag name for git commit sha
	TraceTagCommitSha = "_dd.git.commit.sha"
	// TraceTagGoPath specifies the trace tag name for go module path
	TraceTagGoPath = "_dd.go_path"
)

var (
	lock = sync.Mutex{}

	gitMetadataTags map[string]string
)

func updateTags(tags map[string]string, key string, value string) {
	if _, ok := tags[key]; !ok && value != "" {
		tags[key] = value
	}
}

func updateAllTags(tags map[string]string, newtags map[string]string) {
	for k, v := range newtags {
		updateTags(tags, k, v)
	}
}

// Get git metadata from environment variables
func getTagsFromEnv() map[string]string {
	return map[string]string{
		TagRepositoryURL: removeCredentials(os.Getenv(EnvGitRepositoryURL)),
		TagCommitSha:     os.Getenv(EnvGitCommitSha),
	}
}

// Get git metadata from DD_TAGS
func getTagsFromDDTags() map[string]string {
	etags := ParseTagString(os.Getenv(EnvDDTags))

	return map[string]string{
		TagRepositoryURL: removeCredentials(etags[TagRepositoryURL]),
		TagCommitSha:     etags[TagCommitSha],
		TagGoPath:        etags[TagGoPath],
	}
}

// GetGitMetadataTags returns git metadata tags
func GetGitMetadataTags() map[string]string {
	lock.Lock()
	defer lock.Unlock()

	if gitMetadataTags != nil {
		return gitMetadataTags
	}

	gitMetadataTags = make(map[string]string)

	if BoolEnv(EnvGitMetadataEnabledFlag, true) {
		updateAllTags(gitMetadataTags, getTagsFromEnv())
		updateAllTags(gitMetadataTags, getTagsFromDDTags())
		updateAllTags(gitMetadataTags, getTagsFromBinary())
	}

	return gitMetadataTags
}

// ResetGitMetadataTags reset cashed metadata tags
func ResetGitMetadataTags() {
	lock.Lock()
	defer lock.Unlock()

	gitMetadataTags = nil
}

// CleanGitMetadataTags cleans up tags from git metadata
func CleanGitMetadataTags(tags map[string]string) {
	delete(tags, TagRepositoryURL)
	delete(tags, TagCommitSha)
	delete(tags, TagGoPath)
}

// GetTracerGitMetadataTags returns git metadata tags for tracer
// NB: Currently tracer inject tags with some workaround
// (only with _dd prefix and only for the first span in payload)
// So we provide different tag names
func GetTracerGitMetadataTags() map[string]string {
	res := make(map[string]string)
	tags := GetGitMetadataTags()

	updateTags(res, TraceTagRepositoryURL, tags[TagRepositoryURL])
	updateTags(res, TraceTagCommitSha, tags[TagCommitSha])
	updateTags(res, TraceTagGoPath, tags[TagGoPath])

	return res
}

// removeCredentials returns the passed url with potential credentials removed.
// If the input string is not a valid URL, the string is returned as is.
func removeCredentials(urlStr string) string {
	if urlStr == "" {
		return urlStr
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		// not an url, nothing to remove
		return urlStr
	}
	if u.User == nil {
		// nothing to remove
		return urlStr
	}
	u.User = nil
	return u.String()
}
