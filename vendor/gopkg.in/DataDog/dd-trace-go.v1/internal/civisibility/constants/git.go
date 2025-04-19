// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package constants

const (
	// GitBranch indicates the current git branch.
	// This constant is used to tag traces with the branch name being used in the CI/CD process.
	GitBranch = "git.branch"

	// GitCommitAuthorDate indicates the git commit author date related to the build.
	// This constant is used to tag traces with the date when the author created the commit.
	GitCommitAuthorDate = "git.commit.author.date"

	// GitCommitAuthorEmail indicates the git commit author email related to the build.
	// This constant is used to tag traces with the email of the author who created the commit.
	GitCommitAuthorEmail = "git.commit.author.email"

	// GitCommitAuthorName indicates the git commit author name related to the build.
	// This constant is used to tag traces with the name of the author who created the commit.
	GitCommitAuthorName = "git.commit.author.name"

	// GitCommitCommitterDate indicates the git commit committer date related to the build.
	// This constant is used to tag traces with the date when the committer applied the commit.
	GitCommitCommitterDate = "git.commit.committer.date"

	// GitCommitCommitterEmail indicates the git commit committer email related to the build.
	// This constant is used to tag traces with the email of the committer who applied the commit.
	GitCommitCommitterEmail = "git.commit.committer.email"

	// GitCommitCommitterName indicates the git commit committer name related to the build.
	// This constant is used to tag traces with the name of the committer who applied the commit.
	GitCommitCommitterName = "git.commit.committer.name"

	// GitCommitMessage indicates the git commit message related to the build.
	// This constant is used to tag traces with the message associated with the commit.
	GitCommitMessage = "git.commit.message"

	// GitCommitSHA indicates the git commit SHA1 hash related to the build.
	// This constant is used to tag traces with the SHA1 hash of the commit.
	GitCommitSHA = "git.commit.sha"

	// GitRepositoryURL indicates the git repository URL related to the build.
	// This constant is used to tag traces with the URL of the repository where the commit is stored.
	GitRepositoryURL = "git.repository_url"

	// GitTag indicates the current git tag.
	// This constant is used to tag traces with the tag name associated with the current commit.
	GitTag = "git.tag"

	// GitHeadCommit indicates the GIT head commit hash.
	GitHeadCommit = "git.commit.head_sha"

	// GitPrBaseCommit indicates the GIT PR base commit hash.
	GitPrBaseCommit = "git.pull_request.base_branch_sha"

	// GitPrBaseBranch indicates the GIT PR base branch name.
	GitPrBaseBranch = "git.pull_request.base_branch"
)
