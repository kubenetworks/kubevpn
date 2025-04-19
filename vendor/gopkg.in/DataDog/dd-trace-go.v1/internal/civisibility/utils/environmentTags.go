// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package utils

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/constants"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/osinfo"
)

var (
	// ciTags holds the CI/CD environment variable information.
	currentCiTags  map[string]string // currentCiTags holds the CI/CD tags after originalCiTags + addedTags
	originalCiTags map[string]string // originalCiTags holds the original CI/CD tags after all the CMDs
	addedTags      map[string]string // addedTags holds the tags added by the user
	ciTagsMutex    sync.Mutex

	// ciMetrics holds the CI/CD environment numeric variable information
	currentCiMetrics  map[string]float64 // currentCiMetrics holds the CI/CD metrics after originalCiMetrics + addedMetrics
	originalCiMetrics map[string]float64 // originalCiMetrics holds the original CI/CD metrics after all the CMDs
	addedMetrics      map[string]float64 // addedMetrics holds the metrics added by the user
	ciMetricsMutex    sync.Mutex
)

// GetCITags retrieves and caches the CI/CD tags from environment variables.
// It initializes the ciTags map if it is not already initialized.
// This function is thread-safe due to the use of a mutex.
//
// Returns:
//
//	A map[string]string containing the CI/CD tags.
func GetCITags() map[string]string {
	ciTagsMutex.Lock()
	defer ciTagsMutex.Unlock()

	// Return the current tags if they are already initialized
	if currentCiTags != nil {
		return currentCiTags
	}

	if originalCiTags == nil {
		// If the original tags are not initialized, create them
		originalCiTags = createCITagsMap()
	}

	// Create a new map with the added tags
	newTags := maps.Clone(originalCiTags)
	for k, v := range addedTags {
		newTags[k] = v
	}

	// Update the current tags
	currentCiTags = newTags
	return currentCiTags
}

// AddCITags adds a new tag to the CI/CD tags map.
func AddCITags(tagName, tagValue string) {
	ciTagsMutex.Lock()
	defer ciTagsMutex.Unlock()

	// Add the tag to the added tags dictionary
	if addedTags == nil {
		addedTags = make(map[string]string)
	}
	addedTags[tagName] = tagValue

	// Reset the current tags
	currentCiTags = nil
}

// AddCITagsMap adds a new map of tags to the CI/CD tags map.
func AddCITagsMap(tags map[string]string) {
	if tags == nil {
		return
	}

	ciTagsMutex.Lock()
	defer ciTagsMutex.Unlock()

	// Add the tag to the added tags dictionary
	if addedTags == nil {
		addedTags = make(map[string]string)
	}
	for k, v := range tags {
		addedTags[k] = v
	}

	// Reset the current tags
	currentCiTags = nil
}

// ResetCITags resets the CI/CD tags to their original values.
func ResetCITags() {
	ciTagsMutex.Lock()
	defer ciTagsMutex.Unlock()

	originalCiTags = nil
	currentCiTags = nil
	addedTags = nil
}

// GetCIMetrics retrieves and caches the CI/CD metrics from environment variables.
// It initializes the ciMetrics map if it is not already initialized.
// This function is thread-safe due to the use of a mutex.
//
// Returns:
//
//	A map[string]float64 containing the CI/CD metrics.
func GetCIMetrics() map[string]float64 {
	ciMetricsMutex.Lock()
	defer ciMetricsMutex.Unlock()

	// Return the current metrics if they are already initialized
	if currentCiMetrics != nil {
		return currentCiMetrics
	}

	if originalCiMetrics == nil {
		// If the original metrics are not initialized, create them
		originalCiMetrics = createCIMetricsMap()
	}

	// Create a new map with the added metrics
	newMetrics := maps.Clone(originalCiMetrics)
	for k, v := range addedMetrics {
		newMetrics[k] = v
	}

	// Update the current metrics
	currentCiMetrics = newMetrics
	return currentCiMetrics
}

// AddCIMetrics adds a new metric to the CI/CD metrics map.
func AddCIMetrics(metricName string, metricValue float64) {
	ciMetricsMutex.Lock()
	defer ciMetricsMutex.Unlock()

	// Add the metric to the added metrics dictionary
	if addedMetrics == nil {
		addedMetrics = make(map[string]float64)
	}
	addedMetrics[metricName] = metricValue

	// Reset the current metrics
	currentCiMetrics = nil
}

// AddCIMetricsMap adds a new map of metrics to the CI/CD metrics map.
func AddCIMetricsMap(metrics map[string]float64) {
	if metrics == nil {
		return
	}

	ciMetricsMutex.Lock()
	defer ciMetricsMutex.Unlock()

	// Add the metric to the added metrics dictionary
	if addedMetrics == nil {
		addedMetrics = make(map[string]float64)
	}
	for k, v := range metrics {
		addedMetrics[k] = v
	}

	// Reset the current metrics
	currentCiMetrics = nil
}

// ResetCIMetrics resets the CI/CD metrics to their original values.
func ResetCIMetrics() {
	ciMetricsMutex.Lock()
	defer ciMetricsMutex.Unlock()

	originalCiMetrics = nil
	currentCiMetrics = nil
	addedMetrics = nil
}

// GetRelativePathFromCITagsSourceRoot calculates the relative path from the CI workspace root to the specified path.
// If the CI workspace root is not available in the tags, it returns the original path.
//
// Parameters:
//
//	path - The absolute or relative file path for which the relative path should be calculated.
//
// Returns:
//
//	The relative path from the CI workspace root to the specified path, or the original path if an error occurs.
func GetRelativePathFromCITagsSourceRoot(path string) string {
	tags := GetCITags()
	if v, ok := tags[constants.CIWorkspacePath]; ok {
		relPath, err := filepath.Rel(v, path)
		if err == nil {
			return filepath.ToSlash(relPath)
		}
	}

	return path
}

// createCITagsMap creates a map of CI/CD tags by extracting information from environment variables and the local Git repository.
// It also adds OS and runtime information to the tags.
//
// Returns:
//
//	A map[string]string containing the extracted CI/CD tags.
func createCITagsMap() map[string]string {
	localTags := getProviderTags()

	// Populate runtime values
	localTags[constants.OSPlatform] = runtime.GOOS
	localTags[constants.OSVersion] = osinfo.OSVersion()
	localTags[constants.OSArchitecture] = runtime.GOARCH
	localTags[constants.RuntimeName] = runtime.Compiler
	localTags[constants.RuntimeVersion] = runtime.Version()
	log.Debug("civisibility: os platform: %v", runtime.GOOS)
	log.Debug("civisibility: os architecture: %v", runtime.GOARCH)
	log.Debug("civisibility: runtime version: %v", runtime.Version())

	// Get command line test command
	var cmd string
	if len(os.Args) == 1 {
		cmd = filepath.Base(os.Args[0])
	} else {
		cmd = fmt.Sprintf("%s %s ", filepath.Base(os.Args[0]), strings.Join(os.Args[1:], " "))
	}

	// Filter out some parameters to make the command more stable.
	cmd = regexp.MustCompile(`(?si)-test.gocoverdir=(.*)\s`).ReplaceAllString(cmd, "")
	cmd = regexp.MustCompile(`(?si)-test.v=(.*)\s`).ReplaceAllString(cmd, "")
	cmd = regexp.MustCompile(`(?si)-test.testlogfile=(.*)\s`).ReplaceAllString(cmd, "")
	cmd = strings.TrimSpace(cmd)
	localTags[constants.TestCommand] = cmd
	log.Debug("civisibility: test command: %v", cmd)

	// Populate the test session name
	if testSessionName, ok := os.LookupEnv(constants.CIVisibilityTestSessionNameEnvironmentVariable); ok {
		localTags[constants.TestSessionName] = testSessionName
	} else if jobName, ok := localTags[constants.CIJobName]; ok {
		localTags[constants.TestSessionName] = fmt.Sprintf("%s-%s", jobName, cmd)
	} else {
		localTags[constants.TestSessionName] = cmd
	}
	log.Debug("civisibility: test session name: %v", localTags[constants.TestSessionName])

	// Check if the user provided the test service
	if ddService := os.Getenv("DD_SERVICE"); ddService != "" {
		localTags[constants.UserProvidedTestServiceTag] = "true"
	} else {
		localTags[constants.UserProvidedTestServiceTag] = "false"
	}

	// Populate missing git data
	gitData, _ := getLocalGitData()

	// Populate Git metadata from the local Git repository if not already present in localTags
	if _, ok := localTags[constants.CIWorkspacePath]; !ok {
		localTags[constants.CIWorkspacePath] = gitData.SourceRoot
	}
	if _, ok := localTags[constants.GitRepositoryURL]; !ok {
		localTags[constants.GitRepositoryURL] = gitData.RepositoryURL
	}
	if _, ok := localTags[constants.GitCommitSHA]; !ok {
		localTags[constants.GitCommitSHA] = gitData.CommitSha
	}
	if _, ok := localTags[constants.GitBranch]; !ok {
		localTags[constants.GitBranch] = gitData.Branch
	}

	// If the commit SHA matches, populate additional Git metadata
	if localTags[constants.GitCommitSHA] == gitData.CommitSha {
		if _, ok := localTags[constants.GitCommitAuthorDate]; !ok {
			localTags[constants.GitCommitAuthorDate] = gitData.AuthorDate.String()
		}
		if _, ok := localTags[constants.GitCommitAuthorName]; !ok {
			localTags[constants.GitCommitAuthorName] = gitData.AuthorName
		}
		if _, ok := localTags[constants.GitCommitAuthorEmail]; !ok {
			localTags[constants.GitCommitAuthorEmail] = gitData.AuthorEmail
		}
		if _, ok := localTags[constants.GitCommitCommitterDate]; !ok {
			localTags[constants.GitCommitCommitterDate] = gitData.CommitterDate.String()
		}
		if _, ok := localTags[constants.GitCommitCommitterName]; !ok {
			localTags[constants.GitCommitCommitterName] = gitData.CommitterName
		}
		if _, ok := localTags[constants.GitCommitCommitterEmail]; !ok {
			localTags[constants.GitCommitCommitterEmail] = gitData.CommitterEmail
		}
		if _, ok := localTags[constants.GitCommitMessage]; !ok {
			localTags[constants.GitCommitMessage] = gitData.CommitMessage
		}
	}

	log.Debug("civisibility: workspace directory: %v", localTags[constants.CIWorkspacePath])
	log.Debug("civisibility: common tags created with %v items", len(localTags))
	return localTags
}

// createCIMetricsMap creates a map of CI/CD tags by extracting information from environment variables and runtime information.
//
// Returns:
//
//	A map[string]float64 containing the metrics extracted
func createCIMetricsMap() map[string]float64 {
	localMetrics := make(map[string]float64)
	localMetrics[constants.LogicalCPUCores] = float64(runtime.NumCPU())

	log.Debug("civisibility: common metrics created with %v items", len(localMetrics))
	return localMetrics
}
