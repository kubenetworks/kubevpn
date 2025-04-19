// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

//go:build unix

package osinfo

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func init() {
	// Change the default values for backwards compatibility on scenarios
	if runtime.GOOS == "linux" {
		osName = "Linux (Unknown Distribution)"
		kernelName = "Linux"
	}

	if runtime.GOOS == "darwin" {
		kernelName = "Darwin"
		out, err := exec.Command("sw_vers", "-productVersion").Output()
		if err != nil {
			return
		}

		osVersion = string(bytes.Trim(out, "\n"))
	}

	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		kernelName = string(bytes.TrimRight(uts.Sysname[:], "\x00"))
		kernelVersion = string(bytes.TrimRight(uts.Version[:], "\x00"))
		kernelRelease = strings.SplitN(strings.TrimRight(string(uts.Release[:]), "\x00"), "-", 2)[0]

		// Backwards compatibility on how data is reported for freebsd
		if runtime.GOOS == "freebsd" {
			osVersion = kernelRelease
		}
	}

	f, err := os.Open("/etc/os-release")
	if err != nil {
		return
	}

	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "=", 2)
		switch parts[0] {
		case "NAME":
			osName = strings.Trim(parts[1], "\"")
		case "VERSION":
			osVersion = strings.Trim(parts[1], "\"")
		case "VERSION_ID":
			if osVersion == "" { // Fallback to VERSION_ID if VERSION is not set
				osVersion = strings.Trim(parts[1], "\"")
			}
		}
	}
}
