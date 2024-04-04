// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build ((darwin && (amd64 || arm64)) || (linux && (amd64 || arm64))) && !go1.23 && !datadog.no_waf && (cgo || appsec)

package lib

import (
	"fmt"
	"os"

	_ "embed"
)

//go:embed .version
var EmbeddedWAFVersion string

func DumpEmbeddedWAF() (path string, err error) {
	file, err := os.CreateTemp("", embedNamePattern)
	if err != nil {
		return path, fmt.Errorf("error creating temp file: %w", err)
	}
	path = file.Name()

	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			if err != nil {
				// TODO: rely on errors.Join() once go1.20 is our min supported Go version
				err = fmt.Errorf("%w; along with an error while releasingclosing the temporary file: %v", err, closeErr)
			} else {
				err = fmt.Errorf("error closing file: %w", closeErr)
			}
		}
		if path != "" && err != nil {
			if rmErr := os.Remove(path); rmErr != nil {
				// TODO: rely on errors.Join() once go1.20 is our min supported Go version
				err = fmt.Errorf("%w; along with an error while releasingclosing the temporary file: %v", err, rmErr)
			}
		}
	}()

	if err := os.WriteFile(file.Name(), libddwaf, 0400); err != nil {
		return path, fmt.Errorf("error writing file: %w", err)
	}

	return path, nil
}
