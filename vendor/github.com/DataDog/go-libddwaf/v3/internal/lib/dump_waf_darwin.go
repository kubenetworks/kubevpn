// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build darwin && (amd64 || arm64) && !go1.24 && !datadog.no_waf && (cgo || appsec)

package lib

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
)

// DumpEmbeddedWAF for darwin platform.
// DumpEmbeddedWAF creates a temporary file with the embedded WAF library content and returns the path to the file,
// a closer function and an error. This is the only way to make all implementations of DumpEmbeddedWAF consistent
// across all platforms.
func DumpEmbeddedWAF() (path string, closer func() error, err error) {
	file, err := os.CreateTemp("", "libddwaf-*.dylib")
	if err != nil {
		return "", nil, fmt.Errorf("error creating temp file: %w", err)
	}

	defer func() {
		if err != nil {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("error closing file: %w", closeErr))
			}
			if rmErr := os.Remove(file.Name()); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("error removing file: %w", rmErr))
			}
		}
	}()

	gr, err := gzip.NewReader(bytes.NewReader(libddwaf))
	if err != nil {
		return "", nil, fmt.Errorf("error creating gzip reader: %w", err)
	}

	if _, err := io.Copy(file, gr); err != nil {
		return "", nil, fmt.Errorf("error copying gzip content to file: %w", err)
	}

	if err := gr.Close(); err != nil {
		return "", nil, fmt.Errorf("error closing gzip reader: %w", err)
	}

	return file.Name(), func() error {
		return errors.Join(file.Close(), os.Remove(file.Name()))
	}, nil
}
