// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package utils

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

var (
	// ignoredFunctionsFromStackTrace array with functions we want to ignore on the final stacktrace (because doesn't add anything useful)
	ignoredFunctionsFromStackTrace = []string{"runtime.gopanic", "runtime.panicmem", "runtime.sigpanic"}
)

// GetModuleAndSuiteName extracts the module name and suite name from a given program counter (pc).
// This function utilizes runtime.FuncForPC to retrieve the full function name associated with the
// program counter, then splits the string to separate the package name from the function name.
//
// Example 1:
//
//	Input:  github.com/DataDog/dd-sdk-go-testing.TestRun
//	Output:
//	   module: github.com/DataDog/dd-sdk-go-testing
//	   suite:  testing_test.go
//
// Example 2:
//
//	Input:  github.com/DataDog/dd-sdk-go-testing.TestRun.func1
//	Output:
//	   module: github.com/DataDog/dd-sdk-go-testing
//	   suite:  testing_test.go
//
// Parameters:
//
//	pc - The program counter for which the module and suite name should be retrieved.
//
// Returns:
//
//	module - The module name extracted from the full function name.
//	suite  - The base name of the file where the function is located.
func GetModuleAndSuiteName(pc uintptr) (module string, suite string) {
	funcValue := runtime.FuncForPC(pc)
	funcFullName := funcValue.Name()
	lastSlash := strings.LastIndexByte(funcFullName, '/')
	if lastSlash < 0 {
		lastSlash = 0
	}
	firstDot := strings.IndexByte(funcFullName[lastSlash:], '.') + lastSlash
	file, _ := funcValue.FileLine(funcValue.Entry())
	return funcFullName[:firstDot], filepath.Base(file)
}

// GetStacktrace retrieves the current stack trace, skipping a specified number of frames.
//
// This function captures the stack trace of the current goroutine, formats it, and returns it as a string.
// It uses runtime.Callers to capture the program counters of the stack frames and runtime.CallersFrames
// to convert these program counters into readable frames. The stack trace is formatted to include the function
// name, file name, and line number of each frame.
//
// Parameters:
//
//	skip - The number of stack frames to skip before capturing the stack trace.
//
// Returns:
//
//	A string representation of the current stack trace, with each frame on a new line.
func GetStacktrace(skip int) string {
	pcs := make([]uintptr, 256)
	total := runtime.Callers(skip+2, pcs)
	frames := runtime.CallersFrames(pcs[:total])
	buffer := new(bytes.Buffer)
	for {
		if frame, ok := frames.Next(); ok {
			// let's check if we need to ignore this frame
			if slices.Contains(ignoredFunctionsFromStackTrace, frame.Function) {
				continue
			}
			// writing frame to the buffer
			_, _ = fmt.Fprintf(buffer, "%s\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		} else {
			break
		}

	}
	return buffer.String()
}
