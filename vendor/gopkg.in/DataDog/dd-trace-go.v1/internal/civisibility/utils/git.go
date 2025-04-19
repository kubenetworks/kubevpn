// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package utils

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/civisibility/utils/telemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// MaxPackFileSizeInMb is the maximum size of a pack file in megabytes.
const MaxPackFileSizeInMb = 3

// localGitData holds various pieces of information about the local Git repository,
// including the source root, repository URL, branch, commit SHA, author and committer details, and commit message.
type localGitData struct {
	SourceRoot     string
	RepositoryURL  string
	Branch         string
	CommitSha      string
	AuthorDate     time.Time
	AuthorName     string
	AuthorEmail    string
	CommitterDate  time.Time
	CommitterName  string
	CommitterEmail string
	CommitMessage  string
}

var (
	// regexpSensitiveInfo is a regular expression used to match and filter out sensitive information from URLs.
	regexpSensitiveInfo = regexp.MustCompile("(https?://|ssh?://)[^/]*@")

	// isGitFoundValue is a boolean flag indicating whether the Git executable is available on the system.
	isGitFoundValue bool

	// gitFinder is a sync.Once instance used to ensure that the Git executable is only checked once.
	gitFinder sync.Once
)

// isGitFound checks if the Git executable is available on the system.
func isGitFound() bool {
	gitFinder.Do(func() {
		_, err := exec.LookPath("git")
		isGitFoundValue = err == nil
		if err != nil {
			log.Debug("civisibility.git: git executable not found")
		}
	})
	return isGitFoundValue
}

// execGit executes a Git command with the given arguments.
func execGit(commandType telemetry.CommandType, args ...string) (val []byte, err error) {
	if commandType != telemetry.NotSpecifiedCommandsType {
		startTime := time.Now()
		telemetry.GitCommand(commandType)
		defer func() {
			telemetry.GitCommandMs(commandType, float64(time.Since(startTime).Milliseconds()))
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				switch exitErr.ExitCode() {
				case -1:
					telemetry.GitCommandErrors(commandType, telemetry.ECMinus1CommandExitCode)
				case 1:
					telemetry.GitCommandErrors(commandType, telemetry.EC1CommandExitCode)
				case 2:
					telemetry.GitCommandErrors(commandType, telemetry.EC2CommandExitCode)
				case 127:
					telemetry.GitCommandErrors(commandType, telemetry.EC127CommandExitCode)
				case 128:
					telemetry.GitCommandErrors(commandType, telemetry.EC128CommandExitCode)
				case 129:
					telemetry.GitCommandErrors(commandType, telemetry.EC129CommandExitCode)
				default:
					telemetry.GitCommandErrors(commandType, telemetry.UnknownCommandExitCode)
				}
			} else if err != nil {
				telemetry.GitCommandErrors(commandType, telemetry.MissingCommandExitCode)
			}
		}()
	}
	if !isGitFound() {
		return nil, errors.New("git executable not found")
	}
	return exec.Command("git", args...).CombinedOutput()
}

// execGitString executes a Git command with the given arguments and returns the output as a string.
func execGitString(commandType telemetry.CommandType, args ...string) (string, error) {
	out, err := execGit(commandType, args...)
	strOut := strings.TrimSpace(strings.Trim(string(out), "\n"))
	return strOut, err
}

// execGitStringWithInput executes a Git command with the given input and arguments and returns the output as a string.
func execGitStringWithInput(commandType telemetry.CommandType, input string, args ...string) (val string, err error) {
	if commandType != telemetry.NotSpecifiedCommandsType {
		telemetry.GitCommand(commandType)
		defer func() {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				switch exitErr.ExitCode() {
				case -1:
					telemetry.GitCommandErrors(commandType, telemetry.ECMinus1CommandExitCode)
				case 1:
					telemetry.GitCommandErrors(commandType, telemetry.EC1CommandExitCode)
				case 2:
					telemetry.GitCommandErrors(commandType, telemetry.EC2CommandExitCode)
				case 127:
					telemetry.GitCommandErrors(commandType, telemetry.EC127CommandExitCode)
				case 128:
					telemetry.GitCommandErrors(commandType, telemetry.EC128CommandExitCode)
				case 129:
					telemetry.GitCommandErrors(commandType, telemetry.EC129CommandExitCode)
				default:
					telemetry.GitCommandErrors(commandType, telemetry.UnknownCommandExitCode)
				}
			} else if err != nil {
				telemetry.GitCommandErrors(commandType, telemetry.MissingCommandExitCode)
			}
		}()
	}
	cmd := exec.Command("git", args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	strOut := strings.TrimSpace(strings.Trim(string(out), "\n"))
	return strOut, err
}

// getGitVersion retrieves the version of the Git executable installed on the system.
func getGitVersion() (major int, minor int, patch int, error error) {
	out, err := execGitString(telemetry.NotSpecifiedCommandsType, "--version")
	if err != nil {
		return 0, 0, 0, err
	}
	out = strings.TrimSpace(strings.ReplaceAll(out, "git version ", ""))
	versionParts := strings.Split(out, ".")
	if len(versionParts) < 3 {
		return 0, 0, 0, errors.New("invalid git version")
	}
	major, _ = strconv.Atoi(versionParts[0])
	minor, _ = strconv.Atoi(versionParts[1])
	patch, _ = strconv.Atoi(versionParts[2])
	return major, minor, patch, nil
}

// getLocalGitData retrieves information about the local Git repository from the current HEAD.
// It gathers details such as the repository URL, current branch, latest commit SHA, author and committer details, and commit message.
//
// Returns:
//
//	A localGitData struct populated with the retrieved Git data.
//	An error if any Git command fails or the retrieved data is incomplete.
func getLocalGitData() (localGitData, error) {
	gitData := localGitData{}

	if !isGitFound() {
		return gitData, errors.New("git executable not found")
	}

	// Ensure we have permissions to read the git directory
	if currentDir, err := os.Getwd(); err == nil {
		if gitDir, err := getParentGitFolder(currentDir); err == nil && gitDir != "" {
			log.Debug("civisibility.git: setting permissions to git folder: %s", gitDir)
			if out, err := execGitString(telemetry.NotSpecifiedCommandsType, "config", "--global", "--add", "safe.directory", gitDir); err != nil {
				log.Debug("civisibility.git: error while setting permissions to git folder: %s\n%s\n%s", gitDir, err.Error(), out)
			}
		} else {
			log.Debug("civisibility.git: error getting the parent git folder.")
		}
	} else {
		log.Debug("civisibility.git: error getting the current working directory.")
	}

	// Extract the absolute path to the Git directory
	log.Debug("civisibility.git: getting the absolute path to the Git directory")
	out, err := execGitString(telemetry.NotSpecifiedCommandsType, "rev-parse", "--absolute-git-dir")
	if err == nil {
		gitData.SourceRoot = strings.ReplaceAll(out, ".git", "")
	}

	// Extract the repository URL
	log.Debug("civisibility.git: getting the repository URL")
	out, err = execGitString(telemetry.GetRepositoryCommandsType, "ls-remote", "--get-url")
	if err == nil {
		gitData.RepositoryURL = filterSensitiveInfo(out)
	}

	// Extract the current branch name
	log.Debug("civisibility.git: getting the current branch name")
	out, err = execGitString(telemetry.GetBranchCommandsType, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		gitData.Branch = out
	}

	// Get commit details from the latest commit using git log (git log -1 --pretty='%H","%aI","%an","%ae","%cI","%cn","%ce","%B')
	log.Debug("civisibility.git: getting the latest commit details")
	out, err = execGitString(telemetry.NotSpecifiedCommandsType, "log", "-1", "--pretty=%H\",\"%at\",\"%an\",\"%ae\",\"%ct\",\"%cn\",\"%ce\",\"%B")
	if err != nil {
		return gitData, err
	}

	// Split the output into individual components
	outArray := strings.Split(out, "\",\"")
	if len(outArray) < 8 {
		return gitData, errors.New("git log failed")
	}

	// Parse author and committer dates from Unix timestamp
	authorUnixDate, _ := strconv.ParseInt(outArray[1], 10, 64)
	committerUnixDate, _ := strconv.ParseInt(outArray[4], 10, 64)

	// Populate the localGitData struct with the parsed information
	gitData.CommitSha = outArray[0]
	gitData.AuthorDate = time.Unix(authorUnixDate, 0)
	gitData.AuthorName = outArray[2]
	gitData.AuthorEmail = outArray[3]
	gitData.CommitterDate = time.Unix(committerUnixDate, 0)
	gitData.CommitterName = outArray[5]
	gitData.CommitterEmail = outArray[6]
	gitData.CommitMessage = strings.Trim(outArray[7], "\n")
	return gitData, nil
}

// GetLastLocalGitCommitShas retrieves the commit SHAs of the last 1000 commits in the local Git repository.
func GetLastLocalGitCommitShas() []string {
	// git log --format=%H -n 1000 --since=\"1 month ago\"
	log.Debug("civisibility.git: getting the commit SHAs of the last 1000 commits in the local Git repository")
	out, err := execGitString(telemetry.GetLocalCommitsCommandsType, "log", "--format=%H", "-n", "1000", "--since=\"1 month ago\"")
	if err != nil || out == "" {
		return []string{}
	}
	return strings.Split(out, "\n")
}

// UnshallowGitRepository converts a shallow clone into a complete clone by fetching all missing commits without git content (only commits and tree objects).
func UnshallowGitRepository() (bool, error) {

	// let's do a first check to see if the repository is a shallow clone
	log.Debug("civisibility.unshallow: checking if the repository is a shallow clone")
	isAShallowClone, err := isAShallowCloneRepository()
	if err != nil {
		return false, fmt.Errorf("civisibility.unshallow: error checking if the repository is a shallow clone: %s", err.Error())
	}

	// if the git repo is not a shallow clone, we can return early
	if !isAShallowClone {
		log.Debug("civisibility.unshallow: the repository is not a shallow clone")
		return false, nil
	}

	// the git repo is a shallow clone, we need to double check if there are more than just 1 commit in the logs.
	log.Debug("civisibility.unshallow: the repository is a shallow clone, checking if there are more than one commit in the logs")
	hasMoreThanOneCommits, err := hasTheGitLogHaveMoreThanOneCommits()
	if err != nil {
		return false, fmt.Errorf("civisibility.unshallow: error checking if the git log has more than one commit: %s", err.Error())
	}

	// if there are more than 1 commits, we can return early
	if hasMoreThanOneCommits {
		log.Debug("civisibility.unshallow: the git log has more than one commits")
		return false, nil
	}

	// let's check the git version >= 2.27.0 (git --version) to see if we can unshallow the repository
	log.Debug("civisibility.unshallow: checking the git version")
	major, minor, patch, err := getGitVersion()
	if err != nil {
		return false, fmt.Errorf("civisibility.unshallow: error getting the git version: %s", err.Error())
	}
	log.Debug("civisibility.unshallow: git version: %v.%v.%v", major, minor, patch)
	if major < 2 || (major == 2 && minor < 27) {
		log.Debug("civisibility.unshallow: the git version is less than 2.27.0 we cannot unshallow the repository")
		return false, nil
	}

	// after asking for 2 logs lines, if the git log command returns just one commit sha, we reconfigure the repo
	// to ask for git commits and trees of the last month (no blobs)

	// let's get the origin name (git config --default origin --get clone.defaultRemoteName)
	originName, err := execGitString(telemetry.GetRemoteCommandsType, "config", "--default", "origin", "--get", "clone.defaultRemoteName")
	if err != nil {
		return false, fmt.Errorf("civisibility.unshallow: error getting the origin name: %s\n%s", err.Error(), originName)
	}
	if originName == "" {
		// if the origin name is empty, we fallback to "origin"
		originName = "origin"
	}
	log.Debug("civisibility.unshallow: origin name: %v", originName)

	// let's get the sha of the HEAD (git rev-parse HEAD)
	headSha, err := execGitString(telemetry.GetHeadCommandsType, "rev-parse", "HEAD")
	if err != nil {
		return false, fmt.Errorf("civisibility.unshallow: error getting the HEAD sha: %s\n%s", err.Error(), headSha)
	}
	if headSha == "" {
		// if the HEAD is empty, we fallback to the current branch (git branch --show-current)
		headSha, err = execGitString(telemetry.GetBranchCommandsType, "branch", "--show-current")
		if err != nil {
			return false, fmt.Errorf("civisibility.unshallow: error getting the current branch: %s\n%s", err.Error(), headSha)
		}
	}
	log.Debug("civisibility.unshallow: HEAD sha: %v", headSha)

	// let's fetch the missing commits and trees from the last month
	// git fetch --shallow-since="1 month ago" --update-shallow --filter="blob:none" --recurse-submodules=no $(git config --default origin --get clone.defaultRemoteName) $(git rev-parse HEAD)
	log.Debug("civisibility.unshallow: fetching the missing commits and trees from the last month")
	fetchOutput, err := execGitString(telemetry.UnshallowCommandsType, "fetch", "--shallow-since=\"1 month ago\"", "--update-shallow", "--filter=blob:none", "--recurse-submodules=no", originName, headSha)

	// let's check if the last command was unsuccessful
	if err != nil || fetchOutput == "" {
		log.Debug("civisibility.unshallow: error fetching the missing commits and trees from the last month: %v", err)
		// ***
		// The previous command has a drawback: if the local HEAD is a commit that has not been pushed to the remote, it will fail.
		// If this is the case, we fallback to: `git fetch --shallow-since="1 month ago" --update-shallow --filter="blob:none" --recurse-submodules=no $(git config --default origin --get clone.defaultRemoteName) $(git rev-parse --abbrev-ref --symbolic-full-name @{upstream})`
		// This command will attempt to use the tracked branch for the current branch in order to unshallow.
		// ***

		// let's get the remote branch name: git rev-parse --abbrev-ref --symbolic-full-name @{upstream}
		var remoteBranchName string
		log.Debug("civisibility.unshallow: getting the remote branch name")
		remoteBranchName, err = execGitString(telemetry.UnshallowCommandsType, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
		if err == nil {
			// let's try the alternative: git fetch --shallow-since="1 month ago" --update-shallow --filter="blob:none" --recurse-submodules=no $(git config --default origin --get clone.defaultRemoteName) $(git rev-parse --abbrev-ref --symbolic-full-name @{upstream})
			log.Debug("civisibility.unshallow: fetching the missing commits and trees from the last month using the remote branch name")
			fetchOutput, err = execGitString(telemetry.UnshallowCommandsType, "fetch", "--shallow-since=\"1 month ago\"", "--update-shallow", "--filter=blob:none", "--recurse-submodules=no", originName, remoteBranchName)
		}
	}

	// let's check if the last command was unsuccessful
	if err != nil || fetchOutput == "" {
		log.Debug("civisibility.unshallow: error fetching the missing commits and trees from the last month: %v", err)
		// ***
		// It could be that the CI is working on a detached HEAD or maybe branch tracking hasn’t been set up.
		// In that case, this command will also fail, and we will finally fallback to we just unshallow all the things:
		// `git fetch --shallow-since="1 month ago" --update-shallow --filter="blob:none" --recurse-submodules=no $(git config --default origin --get clone.defaultRemoteName)`
		// ***

		// let's try the last fallback: git fetch --shallow-since="1 month ago" --update-shallow --filter="blob:none" --recurse-submodules=no $(git config --default origin --get clone.defaultRemoteName)
		log.Debug("civisibility.unshallow: fetching the missing commits and trees from the last month using the origin name")
		fetchOutput, err = execGitString(telemetry.UnshallowCommandsType, "fetch", "--shallow-since=\"1 month ago\"", "--update-shallow", "--filter=blob:none", "--recurse-submodules=no", originName)
	}

	if err != nil {
		return false, fmt.Errorf("civisibility.unshallow: error: %s\n%s", err.Error(), fetchOutput)
	}

	log.Debug("civisibility.unshallow: was completed successfully")
	return true, nil
}

// filterSensitiveInfo removes sensitive information from a given URL using a regular expression.
// It replaces the user credentials part of the URL (if present) with an empty string.
//
// Parameters:
//
//	url - The URL string from which sensitive information should be filtered out.
//
// Returns:
//
//	The sanitized URL string with sensitive information removed.
func filterSensitiveInfo(url string) string {
	return string(regexpSensitiveInfo.ReplaceAll([]byte(url), []byte("$1"))[:])
}

// isAShallowCloneRepository checks if the local Git repository is a shallow clone.
func isAShallowCloneRepository() (bool, error) {
	// git rev-parse --is-shallow-repository
	out, err := execGitString(telemetry.CheckShallowCommandsType, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(out) == "true", nil
}

// hasTheGitLogHaveMoreThanOneCommits checks if the local Git repository has more than one commit.
func hasTheGitLogHaveMoreThanOneCommits() (bool, error) {
	// git log --format=oneline -n 2
	out, err := execGitString(telemetry.CheckShallowCommandsType, "log", "--format=oneline", "-n", "2")
	if err != nil || out == "" {
		return false, err
	}

	commitsCount := strings.Count(out, "\n") + 1
	return commitsCount > 1, nil
}

// getObjectsSha get the objects shas from the git repository based on the commits to include and exclude
func getObjectsSha(commitsToInclude []string, commitsToExclude []string) []string {
	// git rev-list --objects --no-object-names --filter=blob:none --since="1 month ago" HEAD " + string.Join(" ", commitsToExclude.Select(c => "^" + c)) + " " + string.Join(" ", commitsToInclude);
	commitsToExcludeArgs := make([]string, len(commitsToExclude))
	for i, c := range commitsToExclude {
		commitsToExcludeArgs[i] = "^" + c
	}
	args := append([]string{"rev-list", "--objects", "--no-object-names", "--filter=blob:none", "--since=\"1 month ago\"", "HEAD"}, append(commitsToExcludeArgs, commitsToInclude...)...)
	out, err := execGitString(telemetry.GetObjectsCommandsType, args...)
	if err != nil {
		return []string{}
	}
	return strings.Split(out, "\n")
}

func CreatePackFiles(commitsToInclude []string, commitsToExclude []string) []string {
	// get the objects shas to send
	objectsShas := getObjectsSha(commitsToInclude, commitsToExclude)
	if len(objectsShas) == 0 {
		log.Debug("civisibility: no objects found to send")
		return nil
	}

	// create the objects shas string
	var objectsShasString string
	for _, objectSha := range objectsShas {
		objectsShasString += objectSha + "\n"
	}

	// get a temporary path to store the pack files
	temporaryPath, err := os.MkdirTemp("", "pack-objects")
	if err != nil {
		log.Warn("civisibility: error creating temporary directory: %s", err)
		return nil
	}

	// git pack-objects --compression=9 --max-pack-size={MaxPackFileSizeInMb}m "{temporaryPath}"
	out, err := execGitStringWithInput(telemetry.PackObjectsCommandsType, objectsShasString,
		"pack-objects", "--compression=9", "--max-pack-size="+strconv.Itoa(MaxPackFileSizeInMb)+"m", temporaryPath+"/")
	if err != nil {
		log.Warn("civisibility: error creating pack files: %s", err)
		return nil
	}

	// construct the full path to the pack files
	var packFiles []string
	for i, packFile := range strings.Split(out, "\n") {
		file := filepath.Join(temporaryPath, fmt.Sprintf("-%s.pack", packFile))

		// check if the pack file exists
		if _, err := os.Stat(file); os.IsNotExist(err) {
			log.Warn("civisibility: pack file not found: %s", packFiles[i])
			continue
		}

		packFiles = append(packFiles, file)
	}

	return packFiles
}

// getParentGitFolder searches from the given directory upwards to find the nearest .git directory.
func getParentGitFolder(innerFolder string) (string, error) {
	if innerFolder == "" {
		return "", nil
	}

	dir := innerFolder
	for {
		gitDirPath := filepath.Join(dir, ".git")
		info, err := os.Stat(gitDirPath)
		if err == nil && info.IsDir() {
			return gitDirPath, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}

		parentDir := filepath.Dir(dir)
		// If we've reached the root directory, stop the loop.
		if parentDir == dir {
			break
		}
		dir = parentDir
	}

	return "", nil
}
