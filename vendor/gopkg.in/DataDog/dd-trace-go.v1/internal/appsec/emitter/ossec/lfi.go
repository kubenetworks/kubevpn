// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2024 Datadog, Inc.

package ossec

import (
	"io/fs"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
)

type (
	// OpenOperation type embodies any kind of function calls that will result in a call to an open(2) syscall
	OpenOperation struct {
		dyngo.Operation
		blockErr error
	}

	// OpenOperationArgs is the arguments for an open operation
	OpenOperationArgs struct {
		// Path is the path to the file to be opened
		Path string
		// Flags are the flags passed to the open(2) syscall
		Flags int
		// Perms are the permissions passed to the open(2) syscall if the creation of a file is required
		Perms fs.FileMode
	}

	// OpenOperationRes is the result of an open operation
	OpenOperationRes[File any] struct {
		// File is the file descriptor returned by the open(2) syscall
		File *File
		// Err is the error returned by the function
		Err *error
	}
)

func (OpenOperationArgs) IsArgOf(*OpenOperation)         {}
func (OpenOperationRes[File]) IsResultOf(*OpenOperation) {}
