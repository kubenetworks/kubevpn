// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package utils

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// This code is based on: https://github.com/mitchellh/go-homedir/blob/v1.1.0/homedir.go (MIT License)

// ExpandPath expands a file path that starts with '~' to the user's home directory.
// If the path does not start with '~', it is returned unchanged.
//
// Parameters:
//
//	path - The file path to be expanded.
//
// Returns:
//
//	The expanded file path, with '~' replaced by the user's home directory, if applicable.
func ExpandPath(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}

	// If the second character is not '/' or '\', return the path unchanged
	if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
		return path
	}

	homeFolder := getHomeDir()
	if len(homeFolder) > 0 {
		return filepath.Join(homeFolder, path[1:])
	}

	return path
}

// getHomeDir returns the home directory of the current user.
// The method used to determine the home directory depends on the operating system.
//
// On Windows, it prefers the HOME environment variable, then USERPROFILE, and finally combines HOMEDRIVE and HOMEPATH.
// On Unix-like systems, it prefers the HOME environment variable, and falls back to various shell commands
// to determine the home directory if necessary.
//
// Returns:
//
//	The home directory of the current user.
func getHomeDir() (homeDir string) {
	defer func() {
		log.Debug("civisibility: home directory: %v", homeDir)
	}()

	if runtime.GOOS == "windows" {
		if home := os.Getenv("HOME"); home != "" {
			// First prefer the HOME environment variable
			return home
		}
		if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
			// Prefer the USERPROFILE environment variable
			return userProfile
		}

		homeDrive := os.Getenv("HOMEDRIVE")
		homePath := os.Getenv("HOMEPATH")
		return homeDrive + homePath
	}

	homeEnv := "HOME"
	if runtime.GOOS == "plan9" {
		// On plan9, environment variables are lowercase.
		homeEnv = "home"
	}

	if home := os.Getenv(homeEnv); home != "" {
		// Prefer the HOME environment variable
		return home
	}

	var stdout bytes.Buffer
	if runtime.GOOS == "darwin" {
		// On macOS, use dscl to read the NFSHomeDirectory
		cmd := exec.Command("sh", "-c", `dscl -q . -read /Users/"$(whoami)" NFSHomeDirectory | sed 's/^[^ ]*: //'`)
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil {
			result := strings.TrimSpace(stdout.String())
			if result != "" {
				return result
			}
		}
	} else {
		// On other Unix-like systems, use getent to read the passwd entry for the current user
		cmd := exec.Command("getent", "passwd", strconv.Itoa(os.Getuid()))
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil {
			if passwd := strings.TrimSpace(stdout.String()); passwd != "" {
				// The passwd entry is in the format: username:password:uid:gid:gecos:home:shell
				passwdParts := strings.SplitN(passwd, ":", 7)
				if len(passwdParts) > 5 {
					return passwdParts[5]
				}
			}
		}
	}

	// If all else fails, use the shell to determine the home directory
	stdout.Reset()
	cmd := exec.Command("sh", "-c", "cd && pwd")
	cmd.Stdout = &stdout
	if err := cmd.Run(); err == nil {
		return strings.TrimSpace(stdout.String())
	}

	return ""
}
