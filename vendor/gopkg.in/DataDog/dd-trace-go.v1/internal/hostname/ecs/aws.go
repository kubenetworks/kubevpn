// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package ecs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/cachedfetch"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/httputils"
)

// declare these as vars not const to ease testing
var (
	metadataURL = os.Getenv("ECS_CONTAINER_METADATA_URI_V4")
	timeout     = 300 * time.Millisecond
)

var taskFetcher = cachedfetch.Fetcher{
	Name: "ECS LaunchType",
	Attempt: func(ctx context.Context) (string, error) {
		taskJSON, err := getResponse(ctx, metadataURL+"/task")
		if err != nil {
			return "", fmt.Errorf("failed to get ECS task metadata: %s", err)
		}
		return taskJSON, nil
	},
}

func getResponse(ctx context.Context, url string) (string, error) {
	return httputils.Get(ctx, url, map[string]string{}, timeout)
}

// GetLaunchType gets the launch-type based on the ECS Task metadata endpoint
func GetLaunchType(ctx context.Context) (string, error) {
	taskJSON, err := taskFetcher.Fetch(ctx)
	if err != nil {
		return "", err
	}

	var metadata struct {
		LaunchType string
	}
	if err := json.Unmarshal([]byte(taskJSON), &metadata); err != nil {
		return "", fmt.Errorf("failed to parse ecs task metadata: %s", err)
	}
	return metadata.LaunchType, nil
}
