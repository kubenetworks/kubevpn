// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package log

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
)

// Level replicates the definition of `DDWAF_LOG_LEVEL` from `ddwaf.h`.
type Level int

const (
	LevelTrace Level = iota
	LevelDebug
	LevelInfo
	LevelWarning
	LevelError
	LevelOff
)

// LevelNamed returns the log level corresponding to the given name, or LevelOff
// if the name corresponds to no known log level.
func LevelNamed(name string) Level {
	switch strings.ToLower(name) {
	case "trace":
		return LevelTrace
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarning
	case "error":
		return LevelError
	case "off":
		return LevelOff
	default:
		return LevelOff
	}
}

func (l Level) String() string {
	switch l {
	case LevelTrace:
		return "TRACE"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarning:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelOff:
		return "OFF"
	default:
		return fmt.Sprintf("0x%X", uintptr(l))
	}
}

var filter *regexp.Regexp

func logMessage(level Level, function, file string, line uint, message string) {
	entry := fmt.Sprintf("[%s] libddwaf @ %s:%d (%s): %s", level, file, line, function, message)

	if filter != nil && !filter.MatchString(entry) {
		return
	}

	log.Println(entry)
}

const EnvVarLogLevel = "DD_APPSEC_WAF_LOG_LEVEL"

func init() {
	const envVarFilter = "DD_APPSEC_WAF_LOG_FILTER"

	if val := os.Getenv(EnvVarLogLevel); val == "" {
		// No log level configured, don't even attempt parsing the regexp.
		return
	}

	if val := os.Getenv(envVarFilter); val != "" {
		var err error
		filter, err = regexp.Compile(val)
		if err != nil {
			log.Fatalf("invalid %s value: %v", envVarFilter, err)
		}
	}
}
