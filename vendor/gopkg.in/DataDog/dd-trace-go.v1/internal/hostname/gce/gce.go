// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package gce

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
	metadataURL = "http://169.254.169.254/computeMetadata/v1"
)

var hostnameFetcher = cachedfetch.Fetcher{
	Name: "GCP Hostname",
	Attempt: func(ctx context.Context) (string, error) {
		hostname, err := getResponseWithMaxLength(ctx, metadataURL+"/instance/hostname",
			255)
		if err != nil {
			return "", fmt.Errorf("unable to retrieve hostname from GCE: %s", err)
		}
		return hostname, nil
	},
}

var projectIDFetcher = cachedfetch.Fetcher{
	Name: "GCP Project ID",
	Attempt: func(ctx context.Context) (string, error) {
		projectID, err := getResponseWithMaxLength(ctx,
			metadataURL+"/project/project-id",
			255)
		if err != nil {
			return "", fmt.Errorf("unable to retrieve project ID from GCE: %s", err)
		}
		return projectID, err
	},
}

var nameFetcher = cachedfetch.Fetcher{
	Name: "GCP Instance Name",
	Attempt: func(ctx context.Context) (string, error) {
		return getResponseWithMaxLength(ctx,
			metadataURL+"/instance/name",
			255)
	},
}

// GetCanonicalHostname returns the DD canonical hostname (prefer: <instance-name>.<project-id>, otherwise <hostname>)
func GetCanonicalHostname(ctx context.Context) (string, error) {
	hostname, err := GetHostname(ctx)
	if err != nil {
		return "", err
	}

	instanceAlias, err := getInstanceAlias(ctx, hostname)
	if err != nil {
		return hostname, nil
	}
	return instanceAlias, nil
}

func getInstanceAlias(ctx context.Context, hostname string) (string, error) {
	instanceName, err := nameFetcher.Fetch(ctx)
	if err != nil {
		// If the endpoint is not reachable, fallback on the old way to get the alias.
		// For instance, it happens in GKE, where the metadata server is only a subset
		// of the Compute Engine metadata server.
		// See https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity#gke_mds
		if hostname == "" {
			return "", fmt.Errorf("unable to retrieve instance name and hostname from GCE: %s", err)
		}
		instanceName = strings.SplitN(hostname, ".", 2)[0]
	}

	projectID, err := projectIDFetcher.Fetch(ctx)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s.%s", instanceName, projectID), nil
}

// GetHostname returns the hostname querying GCE Metadata api
func GetHostname(ctx context.Context) (string, error) {
	return hostnameFetcher.Fetch(ctx)
}

func getResponseWithMaxLength(ctx context.Context, endpoint string, maxLength int) (string, error) {
	result, err := getResponse(ctx, endpoint)
	if err != nil {
		return result, err
	}
	if len(result) > maxLength {
		return "", fmt.Errorf("%v gave a response with length > to %v", endpoint, maxLength)
	}
	return result, err
}

func getResponse(ctx context.Context, url string) (string, error) {
	res, err := httputils.Get(ctx, url, map[string]string{"Metadata-Flavor": "Google"}, 1000*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("GCE metadata API error: %s", err)
	}

	// Some cloud platforms will respond with an empty body, causing the agent to assume a faulty hostname
	if len(res) <= 0 {
		return "", fmt.Errorf("empty response body")
	}

	return res, nil
}
