// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package ec2

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/cachedfetch"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/hostname/httputils"
)

// declare these as vars not const to ease testing
var (
	metadataURL     = "http://169.254.169.254/latest/meta-data"
	defaultPrefixes = []string{"ip-", "domu", "ec2amaz-"}

	MaxHostnameSize = 255
)

var instanceIDFetcher = cachedfetch.Fetcher{
	Name: "EC2 InstanceID",
	Attempt: func(ctx context.Context) (string, error) {
		return getMetadataItemWithMaxLength(ctx,
			"/instance-id",
			MaxHostnameSize,
		)
	},
}

// GetInstanceID fetches the instance id for current host from the EC2 metadata API
func GetInstanceID(ctx context.Context) (string, error) {
	return instanceIDFetcher.Fetch(ctx)
}

func getMetadataItemWithMaxLength(ctx context.Context, endpoint string, maxLength int) (string, error) {
	result, err := getMetadataItem(ctx, endpoint)
	if err != nil {
		return result, err
	}
	if len(result) > maxLength {
		return "", fmt.Errorf("%v gave a response with length > to %v", endpoint, maxLength)
	}
	return result, err
}

func getMetadataItem(ctx context.Context, endpoint string) (string, error) {
	return doHTTPRequest(ctx, metadataURL+endpoint)
}

func doHTTPRequest(ctx context.Context, url string) (string, error) {
	headers := map[string]string{}
	// Note: This assumes IMDS v1. IMDS v2 won't work in a containerized app and requires an API Token
	// Users who have disabled IMDS v1 in favor of v2 will get a fallback hostname from a different provider (likely OS).
	return httputils.Get(ctx, url, headers, 300*time.Millisecond)
}

// IsDefaultHostname checks if a hostname is an EC2 default
func IsDefaultHostname(hostname string) bool {
	hostname = strings.ToLower(hostname)
	isDefault := false

	for _, val := range defaultPrefixes {
		isDefault = isDefault || strings.HasPrefix(hostname, val)
	}
	return isDefault
}
