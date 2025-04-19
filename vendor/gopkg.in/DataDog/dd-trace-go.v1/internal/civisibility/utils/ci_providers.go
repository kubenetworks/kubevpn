// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/constants"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// providerType defines a function type that returns a map of string key-value pairs.
type providerType = func() map[string]string

// providers maps environment variable names to their corresponding CI provider extraction functions.
var providers = map[string]providerType{
	"APPVEYOR":            extractAppveyor,
	"TF_BUILD":            extractAzurePipelines,
	"BITBUCKET_COMMIT":    extractBitbucket,
	"BUDDY":               extractBuddy,
	"BUILDKITE":           extractBuildkite,
	"CIRCLECI":            extractCircleCI,
	"GITHUB_SHA":          extractGithubActions,
	"GITLAB_CI":           extractGitlab,
	"JENKINS_URL":         extractJenkins,
	"TEAMCITY_VERSION":    extractTeamcity,
	"TRAVIS":              extractTravis,
	"BITRISE_BUILD_SLUG":  extractBitrise,
	"CF_BUILD_ID":         extractCodefresh,
	"CODEBUILD_INITIATOR": extractAwsCodePipeline,
}

// getEnvVarsJSON returns a JSON representation of the specified environment variables.
func getEnvVarsJSON(envVars ...string) ([]byte, error) {
	envVarsMap := make(map[string]string)
	for _, envVar := range envVars {
		value := os.Getenv(envVar)
		if value != "" {
			envVarsMap[envVar] = value
		}
	}
	return json.Marshal(envVarsMap)
}

// getProviderTags extracts CI information from environment variables.
func getProviderTags() map[string]string {
	tags := map[string]string{}
	for key, provider := range providers {
		if _, ok := os.LookupEnv(key); !ok {
			continue
		}
		tags = provider()
	}

	// replace with user specific tags
	replaceWithUserSpecificTags(tags)

	// Normalize tags
	normalizeTags(tags)

	// Expand ~
	if tag, ok := tags[constants.CIWorkspacePath]; ok && tag != "" {
		tags[constants.CIWorkspacePath] = ExpandPath(tag)
	}

	// remove empty values
	for tag, value := range tags {
		if value == "" {
			delete(tags, tag)
		}
	}

	if log.DebugEnabled() {
		if providerName, ok := tags[constants.CIProviderName]; ok {
			log.Debug("civisibility: detected ci provider: %v", providerName)
		} else {
			log.Debug("civisibility: no ci provider was detected.")
		}
	}

	return tags
}

// normalizeTags normalizes specific tags to remove prefixes and sensitive information.
func normalizeTags(tags map[string]string) {
	if tag, ok := tags[constants.GitBranch]; ok && tag != "" {
		if strings.Contains(tag, "refs/tags") || strings.Contains(tag, "origin/tags") || strings.Contains(tag, "refs/heads/tags") {
			tags[constants.GitTag] = normalizeRef(tag)
		}
		tags[constants.GitBranch] = normalizeRef(tag)
	}
	if tag, ok := tags[constants.GitTag]; ok && tag != "" {
		tags[constants.GitTag] = normalizeRef(tag)
	}
	if tag, ok := tags[constants.GitRepositoryURL]; ok && tag != "" {
		tags[constants.GitRepositoryURL] = filterSensitiveInfo(tag)
	}
	if tag, ok := tags[constants.CIPipelineURL]; ok && tag != "" {
		tags[constants.CIPipelineURL] = filterSensitiveInfo(tag)
	}
	if tag, ok := tags[constants.CIJobURL]; ok && tag != "" {
		tags[constants.CIJobURL] = filterSensitiveInfo(tag)
	}
	if tag, ok := tags[constants.CIEnvVars]; ok && tag != "" {
		tags[constants.CIEnvVars] = filterSensitiveInfo(tag)
	}
}

// replaceWithUserSpecificTags replaces certain tags with user-specific environment variable values.
func replaceWithUserSpecificTags(tags map[string]string) {
	replace := func(tagName, envName string) {
		tags[tagName] = getEnvironmentVariableIfIsNotEmpty(envName, tags[tagName])
	}

	replace(constants.GitBranch, "DD_GIT_BRANCH")
	replace(constants.GitTag, "DD_GIT_TAG")
	replace(constants.GitRepositoryURL, "DD_GIT_REPOSITORY_URL")
	replace(constants.GitCommitSHA, "DD_GIT_COMMIT_SHA")
	replace(constants.GitCommitMessage, "DD_GIT_COMMIT_MESSAGE")
	replace(constants.GitCommitAuthorName, "DD_GIT_COMMIT_AUTHOR_NAME")
	replace(constants.GitCommitAuthorEmail, "DD_GIT_COMMIT_AUTHOR_EMAIL")
	replace(constants.GitCommitAuthorDate, "DD_GIT_COMMIT_AUTHOR_DATE")
	replace(constants.GitCommitCommitterName, "DD_GIT_COMMIT_COMMITTER_NAME")
	replace(constants.GitCommitCommitterEmail, "DD_GIT_COMMIT_COMMITTER_EMAIL")
	replace(constants.GitCommitCommitterDate, "DD_GIT_COMMIT_COMMITTER_DATE")
}

// getEnvironmentVariableIfIsNotEmpty returns the environment variable value if it is not empty, otherwise returns the default value.
func getEnvironmentVariableIfIsNotEmpty(key string, defaultValue string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return defaultValue
}

// normalizeRef normalizes a Git reference name by removing common prefixes.
func normalizeRef(name string) string {
	// Define the prefixes to remove
	prefixes := []string{"refs/heads/", "refs/", "origin/", "tags/"}

	// Iterate over prefixes and remove them if present
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
		}
	}
	return name
}

// firstEnv returns the value of the first non-empty environment variable from the provided list.
func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

// extractAppveyor extracts CI information specific to Appveyor.
func extractAppveyor() map[string]string {
	tags := map[string]string{}
	url := fmt.Sprintf("https://ci.appveyor.com/project/%s/builds/%s", os.Getenv("APPVEYOR_REPO_NAME"), os.Getenv("APPVEYOR_BUILD_ID"))
	tags[constants.CIProviderName] = "appveyor"
	if os.Getenv("APPVEYOR_REPO_PROVIDER") == "github" {
		tags[constants.GitRepositoryURL] = fmt.Sprintf("https://github.com/%s.git", os.Getenv("APPVEYOR_REPO_NAME"))
	} else {
		tags[constants.GitRepositoryURL] = os.Getenv("APPVEYOR_REPO_NAME")
	}

	tags[constants.GitCommitSHA] = os.Getenv("APPVEYOR_REPO_COMMIT")
	tags[constants.GitBranch] = firstEnv("APPVEYOR_PULL_REQUEST_HEAD_REPO_BRANCH", "APPVEYOR_REPO_BRANCH")
	tags[constants.GitTag] = os.Getenv("APPVEYOR_REPO_TAG_NAME")

	tags[constants.CIWorkspacePath] = os.Getenv("APPVEYOR_BUILD_FOLDER")
	tags[constants.CIPipelineID] = os.Getenv("APPVEYOR_BUILD_ID")
	tags[constants.CIPipelineName] = os.Getenv("APPVEYOR_REPO_NAME")
	tags[constants.CIPipelineNumber] = os.Getenv("APPVEYOR_BUILD_NUMBER")
	tags[constants.CIPipelineURL] = url
	tags[constants.CIJobURL] = url
	tags[constants.GitCommitMessage] = fmt.Sprintf("%s\n%s", os.Getenv("APPVEYOR_REPO_COMMIT_MESSAGE"), os.Getenv("APPVEYOR_REPO_COMMIT_MESSAGE_EXTENDED"))
	tags[constants.GitCommitAuthorName] = os.Getenv("APPVEYOR_REPO_COMMIT_AUTHOR")
	tags[constants.GitCommitAuthorEmail] = os.Getenv("APPVEYOR_REPO_COMMIT_AUTHOR_EMAIL")
	return tags
}

// extractAzurePipelines extracts CI information specific to Azure Pipelines.
func extractAzurePipelines() map[string]string {
	tags := map[string]string{}
	baseURL := fmt.Sprintf("%s%s/_build/results?buildId=%s", os.Getenv("SYSTEM_TEAMFOUNDATIONSERVERURI"), os.Getenv("SYSTEM_TEAMPROJECTID"), os.Getenv("BUILD_BUILDID"))
	pipelineURL := baseURL
	jobURL := fmt.Sprintf("%s&view=logs&j=%s&t=%s", baseURL, os.Getenv("SYSTEM_JOBID"), os.Getenv("SYSTEM_TASKINSTANCEID"))
	branchOrTag := firstEnv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "BUILD_SOURCEBRANCH", "BUILD_SOURCEBRANCHNAME")
	branch := ""
	tag := ""
	if strings.Contains(branchOrTag, "tags/") {
		tag = branchOrTag
	} else {
		branch = branchOrTag
	}
	tags[constants.CIProviderName] = "azurepipelines"
	tags[constants.CIWorkspacePath] = os.Getenv("BUILD_SOURCESDIRECTORY")

	tags[constants.CIPipelineID] = os.Getenv("BUILD_BUILDID")
	tags[constants.CIPipelineName] = os.Getenv("BUILD_DEFINITIONNAME")
	tags[constants.CIPipelineNumber] = os.Getenv("BUILD_BUILDID")
	tags[constants.CIPipelineURL] = pipelineURL

	tags[constants.CIStageName] = os.Getenv("SYSTEM_STAGEDISPLAYNAME")

	tags[constants.CIJobName] = os.Getenv("SYSTEM_JOBDISPLAYNAME")
	tags[constants.CIJobURL] = jobURL

	tags[constants.GitRepositoryURL] = firstEnv("SYSTEM_PULLREQUEST_SOURCEREPOSITORYURI", "BUILD_REPOSITORY_URI")
	tags[constants.GitCommitSHA] = firstEnv("SYSTEM_PULLREQUEST_SOURCECOMMITID", "BUILD_SOURCEVERSION")
	tags[constants.GitBranch] = branch
	tags[constants.GitTag] = tag
	tags[constants.GitCommitMessage] = os.Getenv("BUILD_SOURCEVERSIONMESSAGE")
	tags[constants.GitCommitAuthorName] = os.Getenv("BUILD_REQUESTEDFORID")
	tags[constants.GitCommitAuthorEmail] = os.Getenv("BUILD_REQUESTEDFOREMAIL")

	jsonString, err := getEnvVarsJSON("SYSTEM_TEAMPROJECTID", "BUILD_BUILDID", "SYSTEM_JOBID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	return tags
}

// extractBitrise extracts CI information specific to Bitrise.
func extractBitrise() map[string]string {
	tags := map[string]string{}
	tags[constants.CIProviderName] = "bitrise"
	tags[constants.GitRepositoryURL] = os.Getenv("GIT_REPOSITORY_URL")
	tags[constants.GitCommitSHA] = firstEnv("BITRISE_GIT_COMMIT", "GIT_CLONE_COMMIT_HASH")
	tags[constants.GitBranch] = firstEnv("BITRISEIO_GIT_BRANCH_DEST", "BITRISE_GIT_BRANCH")
	tags[constants.GitTag] = os.Getenv("BITRISE_GIT_TAG")
	tags[constants.CIWorkspacePath] = os.Getenv("BITRISE_SOURCE_DIR")
	tags[constants.CIPipelineID] = os.Getenv("BITRISE_BUILD_SLUG")
	tags[constants.CIPipelineName] = os.Getenv("BITRISE_TRIGGERED_WORKFLOW_ID")
	tags[constants.CIPipelineNumber] = os.Getenv("BITRISE_BUILD_NUMBER")
	tags[constants.CIPipelineURL] = os.Getenv("BITRISE_BUILD_URL")
	tags[constants.GitCommitMessage] = os.Getenv("BITRISE_GIT_MESSAGE")
	return tags
}

// extractBitbucket extracts CI information specific to Bitbucket.
func extractBitbucket() map[string]string {
	tags := map[string]string{}
	url := fmt.Sprintf("https://bitbucket.org/%s/addon/pipelines/home#!/results/%s", os.Getenv("BITBUCKET_REPO_FULL_NAME"), os.Getenv("BITBUCKET_BUILD_NUMBER"))
	tags[constants.CIProviderName] = "bitbucket"
	tags[constants.GitRepositoryURL] = firstEnv("BITBUCKET_GIT_SSH_ORIGIN", "BITBUCKET_GIT_HTTP_ORIGIN")
	tags[constants.GitCommitSHA] = os.Getenv("BITBUCKET_COMMIT")
	tags[constants.GitBranch] = os.Getenv("BITBUCKET_BRANCH")
	tags[constants.GitTag] = os.Getenv("BITBUCKET_TAG")
	tags[constants.CIWorkspacePath] = os.Getenv("BITBUCKET_CLONE_DIR")
	tags[constants.CIPipelineID] = strings.Trim(os.Getenv("BITBUCKET_PIPELINE_UUID"), "{}")
	tags[constants.CIPipelineNumber] = os.Getenv("BITBUCKET_BUILD_NUMBER")
	tags[constants.CIPipelineName] = os.Getenv("BITBUCKET_REPO_FULL_NAME")
	tags[constants.CIPipelineURL] = url
	tags[constants.CIJobURL] = url
	return tags
}

// extractBuddy extracts CI information specific to Buddy.
func extractBuddy() map[string]string {
	tags := map[string]string{}
	tags[constants.CIProviderName] = "buddy"
	tags[constants.CIPipelineID] = fmt.Sprintf("%s/%s", os.Getenv("BUDDY_PIPELINE_ID"), os.Getenv("BUDDY_EXECUTION_ID"))
	tags[constants.CIPipelineName] = os.Getenv("BUDDY_PIPELINE_NAME")
	tags[constants.CIPipelineNumber] = os.Getenv("BUDDY_EXECUTION_ID")
	tags[constants.CIPipelineURL] = os.Getenv("BUDDY_EXECUTION_URL")
	tags[constants.GitCommitSHA] = os.Getenv("BUDDY_EXECUTION_REVISION")
	tags[constants.GitRepositoryURL] = os.Getenv("BUDDY_SCM_URL")
	tags[constants.GitBranch] = os.Getenv("BUDDY_EXECUTION_BRANCH")
	tags[constants.GitTag] = os.Getenv("BUDDY_EXECUTION_TAG")
	tags[constants.GitCommitMessage] = os.Getenv("BUDDY_EXECUTION_REVISION_MESSAGE")
	tags[constants.GitCommitCommitterName] = os.Getenv("BUDDY_EXECUTION_REVISION_COMMITTER_NAME")
	tags[constants.GitCommitCommitterEmail] = os.Getenv("BUDDY_EXECUTION_REVISION_COMMITTER_EMAIL")
	return tags
}

// extractBuildkite extracts CI information specific to Buildkite.
func extractBuildkite() map[string]string {
	tags := map[string]string{}
	tags[constants.GitBranch] = os.Getenv("BUILDKITE_BRANCH")
	tags[constants.GitCommitSHA] = os.Getenv("BUILDKITE_COMMIT")
	tags[constants.GitRepositoryURL] = os.Getenv("BUILDKITE_REPO")
	tags[constants.GitTag] = os.Getenv("BUILDKITE_TAG")
	tags[constants.CIPipelineID] = os.Getenv("BUILDKITE_BUILD_ID")
	tags[constants.CIPipelineName] = os.Getenv("BUILDKITE_PIPELINE_SLUG")
	tags[constants.CIPipelineNumber] = os.Getenv("BUILDKITE_BUILD_NUMBER")
	tags[constants.CIPipelineURL] = os.Getenv("BUILDKITE_BUILD_URL")
	tags[constants.CIJobURL] = fmt.Sprintf("%s#%s", os.Getenv("BUILDKITE_BUILD_URL"), os.Getenv("BUILDKITE_JOB_ID"))
	tags[constants.CIProviderName] = "buildkite"
	tags[constants.CIWorkspacePath] = os.Getenv("BUILDKITE_BUILD_CHECKOUT_PATH")
	tags[constants.GitCommitMessage] = os.Getenv("BUILDKITE_MESSAGE")
	tags[constants.GitCommitAuthorName] = os.Getenv("BUILDKITE_BUILD_AUTHOR")
	tags[constants.GitCommitAuthorEmail] = os.Getenv("BUILDKITE_BUILD_AUTHOR_EMAIL")
	tags[constants.CINodeName] = os.Getenv("BUILDKITE_AGENT_ID")

	jsonString, err := getEnvVarsJSON("BUILDKITE_BUILD_ID", "BUILDKITE_JOB_ID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	var extraTags []string
	envVars := os.Environ()
	for _, envVar := range envVars {
		if strings.HasPrefix(envVar, "BUILDKITE_AGENT_META_DATA_") {
			envVarAsTag := envVar
			envVarAsTag = strings.TrimPrefix(envVarAsTag, "BUILDKITE_AGENT_META_DATA_")
			envVarAsTag = strings.ToLower(envVarAsTag)
			envVarAsTag = strings.Replace(envVarAsTag, "=", ":", 1)
			extraTags = append(extraTags, envVarAsTag)
		}
	}

	if len(extraTags) != 0 {
		// HACK: Sorting isn't actually needed, but it simplifies testing if the order is consistent
		sort.Sort(sort.Reverse(sort.StringSlice(extraTags)))
		jsonString, err = json.Marshal(extraTags)
		if err == nil {
			tags[constants.CINodeLabels] = string(jsonString)
		}
	}

	return tags
}

// extractCircleCI extracts CI information specific to CircleCI.
func extractCircleCI() map[string]string {
	tags := map[string]string{}
	tags[constants.CIProviderName] = "circleci"
	tags[constants.GitRepositoryURL] = os.Getenv("CIRCLE_REPOSITORY_URL")
	tags[constants.GitCommitSHA] = os.Getenv("CIRCLE_SHA1")
	tags[constants.GitTag] = os.Getenv("CIRCLE_TAG")
	tags[constants.GitBranch] = os.Getenv("CIRCLE_BRANCH")
	tags[constants.CIWorkspacePath] = os.Getenv("CIRCLE_WORKING_DIRECTORY")
	tags[constants.CIPipelineID] = os.Getenv("CIRCLE_WORKFLOW_ID")
	tags[constants.CIPipelineName] = os.Getenv("CIRCLE_PROJECT_REPONAME")
	tags[constants.CIPipelineNumber] = os.Getenv("CIRCLE_BUILD_NUM")
	tags[constants.CIPipelineURL] = fmt.Sprintf("https://app.circleci.com/pipelines/workflows/%s", os.Getenv("CIRCLE_WORKFLOW_ID"))
	tags[constants.CIJobName] = os.Getenv("CIRCLE_JOB")
	tags[constants.CIJobURL] = os.Getenv("CIRCLE_BUILD_URL")

	jsonString, err := getEnvVarsJSON("CIRCLE_BUILD_NUM", "CIRCLE_WORKFLOW_ID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	return tags
}

// extractGithubActions extracts CI information specific to GitHub Actions.
func extractGithubActions() map[string]string {
	tags := map[string]string{}
	branchOrTag := firstEnv("GITHUB_HEAD_REF", "GITHUB_REF")
	tag := ""
	branch := ""
	if strings.Contains(branchOrTag, "tags/") {
		tag = branchOrTag
	} else {
		branch = branchOrTag
	}

	serverURL := os.Getenv("GITHUB_SERVER_URL")
	if serverURL == "" {
		serverURL = "https://github.com"
	}
	serverURL = strings.TrimSuffix(serverURL, "/")

	rawRepository := fmt.Sprintf("%s/%s", serverURL, os.Getenv("GITHUB_REPOSITORY"))
	pipelineID := os.Getenv("GITHUB_RUN_ID")
	commitSha := os.Getenv("GITHUB_SHA")

	tags[constants.CIProviderName] = "github"
	tags[constants.GitRepositoryURL] = rawRepository + ".git"
	tags[constants.GitCommitSHA] = commitSha
	tags[constants.GitBranch] = branch
	tags[constants.GitTag] = tag
	tags[constants.CIWorkspacePath] = os.Getenv("GITHUB_WORKSPACE")
	tags[constants.CIPipelineID] = pipelineID
	tags[constants.CIPipelineNumber] = os.Getenv("GITHUB_RUN_NUMBER")
	tags[constants.CIPipelineName] = os.Getenv("GITHUB_WORKFLOW")
	tags[constants.CIJobURL] = fmt.Sprintf("%s/commit/%s/checks", rawRepository, commitSha)
	tags[constants.CIJobName] = os.Getenv("GITHUB_JOB")

	attempts := os.Getenv("GITHUB_RUN_ATTEMPT")
	if attempts == "" {
		tags[constants.CIPipelineURL] = fmt.Sprintf("%s/actions/runs/%s", rawRepository, pipelineID)
	} else {
		tags[constants.CIPipelineURL] = fmt.Sprintf("%s/actions/runs/%s/attempts/%s", rawRepository, pipelineID, attempts)
	}

	jsonString, err := getEnvVarsJSON("GITHUB_SERVER_URL", "GITHUB_REPOSITORY", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	// Extract PR information from the github event json file
	eventFilePath := os.Getenv("GITHUB_EVENT_PATH")
	if stats, ok := os.Stat(eventFilePath); ok == nil && !stats.IsDir() {
		if eventFile, err := os.Open(eventFilePath); err == nil {
			defer eventFile.Close()

			var eventJSON struct {
				PullRequest struct {
					Base struct {
						Sha string `json:"sha"`
						Ref string `json:"ref"`
					} `json:"base"`
					Head struct {
						Sha string `json:"sha"`
					} `json:"head"`
				} `json:"pull_request"`
			}

			eventDecoder := json.NewDecoder(eventFile)
			if eventDecoder.Decode(&eventJSON) == nil {
				tags[constants.GitHeadCommit] = eventJSON.PullRequest.Head.Sha
				tags[constants.GitPrBaseCommit] = eventJSON.PullRequest.Base.Sha
				tags[constants.GitPrBaseBranch] = eventJSON.PullRequest.Base.Ref
			}
		}
	}

	// Fallback if GitPrBaseBranch is not set
	if tmpVal, ok := tags[constants.GitPrBaseBranch]; !ok || tmpVal == "" {
		tags[constants.GitPrBaseBranch] = os.Getenv("GITHUB_BASE_REF")
	}

	return tags
}

// extractGitlab extracts CI information specific to GitLab.
func extractGitlab() map[string]string {
	tags := map[string]string{}
	url := os.Getenv("CI_PIPELINE_URL")

	tags[constants.CIProviderName] = "gitlab"
	tags[constants.GitRepositoryURL] = os.Getenv("CI_REPOSITORY_URL")
	tags[constants.GitCommitSHA] = os.Getenv("CI_COMMIT_SHA")
	tags[constants.GitBranch] = firstEnv("CI_COMMIT_BRANCH", "CI_COMMIT_REF_NAME")
	tags[constants.GitTag] = os.Getenv("CI_COMMIT_TAG")
	tags[constants.CIWorkspacePath] = os.Getenv("CI_PROJECT_DIR")
	tags[constants.CIPipelineID] = os.Getenv("CI_PIPELINE_ID")
	tags[constants.CIPipelineName] = os.Getenv("CI_PROJECT_PATH")
	tags[constants.CIPipelineNumber] = os.Getenv("CI_PIPELINE_IID")
	tags[constants.CIPipelineURL] = url
	tags[constants.CIJobURL] = os.Getenv("CI_JOB_URL")
	tags[constants.CIJobName] = os.Getenv("CI_JOB_NAME")
	tags[constants.CIStageName] = os.Getenv("CI_JOB_STAGE")
	tags[constants.GitCommitMessage] = os.Getenv("CI_COMMIT_MESSAGE")
	tags[constants.CINodeName] = os.Getenv("CI_RUNNER_ID")
	tags[constants.CINodeLabels] = os.Getenv("CI_RUNNER_TAGS")

	author := os.Getenv("CI_COMMIT_AUTHOR")
	authorArray := strings.FieldsFunc(author, func(s rune) bool {
		return s == '<' || s == '>'
	})
	tags[constants.GitCommitAuthorName] = strings.TrimSpace(authorArray[0])
	tags[constants.GitCommitAuthorEmail] = strings.TrimSpace(authorArray[1])
	tags[constants.GitCommitAuthorDate] = os.Getenv("CI_COMMIT_TIMESTAMP")

	jsonString, err := getEnvVarsJSON("CI_PROJECT_URL", "CI_PIPELINE_ID", "CI_JOB_ID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	return tags
}

// extractJenkins extracts CI information specific to Jenkins.
func extractJenkins() map[string]string {
	tags := map[string]string{}
	tags[constants.CIProviderName] = "jenkins"
	tags[constants.GitRepositoryURL] = firstEnv("GIT_URL", "GIT_URL_1")
	tags[constants.GitCommitSHA] = os.Getenv("GIT_COMMIT")

	branchOrTag := os.Getenv("GIT_BRANCH")
	empty := []byte("")
	name, hasName := os.LookupEnv("JOB_NAME")

	if strings.Contains(branchOrTag, "tags/") {
		tags[constants.GitTag] = branchOrTag
	} else {
		tags[constants.GitBranch] = branchOrTag
		// remove branch for job name
		removeBranch := regexp.MustCompile(fmt.Sprintf("/%s", normalizeRef(branchOrTag)))
		name = string(removeBranch.ReplaceAll([]byte(name), empty))
	}

	if hasName {
		removeVars := regexp.MustCompile("/[^/]+=[^/]*")
		name = string(removeVars.ReplaceAll([]byte(name), empty))
	}

	tags[constants.CIWorkspacePath] = os.Getenv("WORKSPACE")
	tags[constants.CIPipelineID] = os.Getenv("BUILD_TAG")
	tags[constants.CIPipelineNumber] = os.Getenv("BUILD_NUMBER")
	tags[constants.CIPipelineName] = name
	tags[constants.CIPipelineURL] = os.Getenv("BUILD_URL")
	tags[constants.CINodeName] = os.Getenv("NODE_NAME")

	jsonString, err := getEnvVarsJSON("DD_CUSTOM_TRACE_ID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	nodeLabels := os.Getenv("NODE_LABELS")
	if len(nodeLabels) > 0 {
		labelsArray := strings.Split(nodeLabels, " ")
		jsonString, err := json.Marshal(labelsArray)
		if err == nil {
			tags[constants.CINodeLabels] = string(jsonString)
		}
	}

	return tags
}

// extractTeamcity extracts CI information specific to TeamCity.
func extractTeamcity() map[string]string {
	tags := map[string]string{}
	tags[constants.CIProviderName] = "teamcity"
	tags[constants.CIJobURL] = os.Getenv("BUILD_URL")
	tags[constants.CIJobName] = os.Getenv("TEAMCITY_BUILDCONF_NAME")
	return tags
}

// extractCodefresh extracts CI information specific to Codefresh.
func extractCodefresh() map[string]string {
	tags := map[string]string{}
	tags[constants.CIProviderName] = "codefresh"
	tags[constants.CIPipelineID] = os.Getenv("CF_BUILD_ID")
	tags[constants.CIPipelineName] = os.Getenv("CF_PIPELINE_NAME")
	tags[constants.CIPipelineURL] = os.Getenv("CF_BUILD_URL")
	tags[constants.CIJobName] = os.Getenv("CF_STEP_NAME")

	jsonString, err := getEnvVarsJSON("CF_BUILD_ID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	cfBranch := os.Getenv("CF_BRANCH")
	isTag := strings.Contains(cfBranch, "tags/")
	var refKey string
	if isTag {
		refKey = constants.GitTag
	} else {
		refKey = constants.GitBranch
	}
	tags[refKey] = normalizeRef(cfBranch)

	return tags
}

// extractTravis extracts CI information specific to Travis CI.
func extractTravis() map[string]string {
	tags := map[string]string{}
	prSlug := os.Getenv("TRAVIS_PULL_REQUEST_SLUG")
	repoSlug := prSlug
	if strings.TrimSpace(repoSlug) == "" {
		repoSlug = os.Getenv("TRAVIS_REPO_SLUG")
	}
	tags[constants.CIProviderName] = "travisci"
	tags[constants.GitRepositoryURL] = fmt.Sprintf("https://github.com/%s.git", repoSlug)
	tags[constants.GitCommitSHA] = os.Getenv("TRAVIS_COMMIT")
	tags[constants.GitTag] = os.Getenv("TRAVIS_TAG")
	tags[constants.GitBranch] = firstEnv("TRAVIS_PULL_REQUEST_BRANCH", "TRAVIS_BRANCH")
	tags[constants.CIWorkspacePath] = os.Getenv("TRAVIS_BUILD_DIR")
	tags[constants.CIPipelineID] = os.Getenv("TRAVIS_BUILD_ID")
	tags[constants.CIPipelineNumber] = os.Getenv("TRAVIS_BUILD_NUMBER")
	tags[constants.CIPipelineName] = repoSlug
	tags[constants.CIPipelineURL] = os.Getenv("TRAVIS_BUILD_WEB_URL")
	tags[constants.CIJobURL] = os.Getenv("TRAVIS_JOB_WEB_URL")
	tags[constants.GitCommitMessage] = os.Getenv("TRAVIS_COMMIT_MESSAGE")
	return tags
}

// extractAwsCodePipeline extracts CI information specific to AWS CodePipeline.
func extractAwsCodePipeline() map[string]string {
	tags := map[string]string{}

	if !strings.HasPrefix(os.Getenv("CODEBUILD_INITIATOR"), "codepipeline") {
		// CODEBUILD_INITIATOR is defined but this is not a codepipeline build
		return tags
	}

	tags[constants.CIProviderName] = "awscodepipeline"
	tags[constants.CIPipelineID] = os.Getenv("DD_PIPELINE_EXECUTION_ID")

	jsonString, err := getEnvVarsJSON("CODEBUILD_BUILD_ARN", "DD_ACTION_EXECUTION_ID", "DD_PIPELINE_EXECUTION_ID")
	if err == nil {
		tags[constants.CIEnvVars] = string(jsonString)
	}

	return tags
}
