// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/cachedfetch"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/httputils"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/validate"
)

// declare these as vars not const to ease testing
var (
	metadataURL = "http://169.254.169.254"
	timeout     = 300 * time.Millisecond

	// CloudProviderName contains the inventory name of for Azure
	CloudProviderName = "Azure"
)

func getResponse(ctx context.Context, url string) (string, error) {
	return httputils.Get(ctx, url, map[string]string{"Metadata": "true"}, timeout)
}

// GetHostname returns hostname based on Azure instance metadata.
func GetHostname(ctx context.Context) (string, error) {
	metadataJSON, err := instanceMetaFetcher.Fetch(ctx)
	if err != nil {
		return "", err
	}

	var metadata struct {
		VMID string
	}
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil {
		return "", fmt.Errorf("failed to parse Azure instance metadata: %s", err)
	}

	if err := validate.ValidHostname(metadata.VMID); err != nil {
		return "", err
	}

	return metadata.VMID, nil
}

var instanceMetaFetcher = cachedfetch.Fetcher{
	Name: "Azure Instance Metadata",
	Attempt: func(ctx context.Context) (string, error) {
		metadataJSON, err := getResponse(ctx,
			metadataURL+"/metadata/instance/compute?api-version=2017-08-01")
		if err != nil {
			return "", fmt.Errorf("failed to get Azure instance metadata: %s", err)
		}
		return metadataJSON, nil
	},
}
