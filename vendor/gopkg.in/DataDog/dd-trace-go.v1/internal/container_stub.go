// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

//go:build !linux

package internal

// ContainerID attempts to return the container ID from /proc/self/cgroup or empty on failure.
func ContainerID() string {
	return ""
}

// EntityID attempts to return the container ID or the cgroup v2 node inode if the container ID is not available.
// The cid is prefixed with `cid-` and the inode with `in-`.
func EntityID() string {
	return ""
}
