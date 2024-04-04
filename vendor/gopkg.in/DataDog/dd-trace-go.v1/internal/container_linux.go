// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

//go:build linux

package internal

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
)

const (
	// cgroupPath is the path to the cgroup file where we can find the container id if one exists.
	cgroupPath = "/proc/self/cgroup"

	// cgroupV1BaseController is the base controller used to identify the cgroup v1 mount point in the cgroupMounts map.
	cgroupV1BaseController = "memory"

	// defaultCgroupMountPath is the path to the cgroup mount point.
	defaultCgroupMountPath = "/sys/fs/cgroup"

	uuidSource      = "[0-9a-f]{8}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{12}|[0-9a-f]{8}(?:-[0-9a-f]{4}){4}$"
	containerSource = "[0-9a-f]{64}"
	taskSource      = "[0-9a-f]{32}-\\d+"

	// From https://github.com/torvalds/linux/blob/5859a2b1991101d6b978f3feb5325dad39421f29/include/linux/proc_ns.h#L41-L49
	// Currently, host namespace inode number are hardcoded, which can be used to detect
	// if we're running in host namespace or not (does not work when running in DinD)
	hostCgroupNamespaceInode = 0xEFFFFFFB
)

var (
	// expLine matches a line in the /proc/self/cgroup file. It has a submatch for the last element (path), which contains the container ID.
	expLine = regexp.MustCompile(`^\d+:[^:]*:(.+)$`)

	// expContainerID matches contained IDs and sources. Source: https://github.com/Qard/container-info/blob/master/index.js
	expContainerID = regexp.MustCompile(fmt.Sprintf(`(%s|%s|%s)(?:.scope)?$`, uuidSource, containerSource, taskSource))

	// containerID is the containerID read at init from /proc/self/cgroup
	containerID string

	// entityID is the entityID to use for the container. It is the `cid-<containerID>` if the container id available,
	// otherwise the cgroup node controller's inode prefixed with `in-` or an empty string on incompatible OS.
	// We use the memory controller on cgroupv1 and the root cgroup on cgroupv2.
	entityID string
)

func init() {
	containerID = readContainerID(cgroupPath)
	entityID = readEntityID(defaultCgroupMountPath, cgroupPath, isHostCgroupNamespace())
}

// parseContainerID finds the first container ID reading from r and returns it.
func parseContainerID(r io.Reader) string {
	scn := bufio.NewScanner(r)
	for scn.Scan() {
		path := expLine.FindStringSubmatch(scn.Text())
		if len(path) != 2 {
			// invalid entry, continue
			continue
		}
		if parts := expContainerID.FindStringSubmatch(path[1]); len(parts) == 2 {
			return parts[1]
		}
	}
	return ""
}

// readContainerID attempts to return the container ID from the provided file path or empty on failure.
func readContainerID(fpath string) string {
	f, err := os.Open(fpath)
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseContainerID(f)
}

// ContainerID attempts to return the container ID from /proc/self/cgroup or empty on failure.
func ContainerID() string {
	return containerID
}

// parseCgroupNodePath parses /proc/self/cgroup and returns a map of controller to its associated cgroup node path.
func parseCgroupNodePath(r io.Reader) map[string]string {
	res := make(map[string]string)
	scn := bufio.NewScanner(r)
	for scn.Scan() {
		line := scn.Text()
		tokens := strings.Split(line, ":")
		if len(tokens) != 3 {
			continue
		}
		if tokens[1] == cgroupV1BaseController || tokens[1] == "" {
			res[tokens[1]] = tokens[2]
		}
	}
	return res
}

// getCgroupInode returns the cgroup controller inode if it exists otherwise an empty string.
// The inode is prefixed by "in-" and is used by the agent to retrieve the container ID.
// We first try to retrieve the cgroupv1 memory controller inode, if it fails we try to retrieve the cgroupv2 inode.
func getCgroupInode(cgroupMountPath, procSelfCgroupPath string) string {
	// Parse /proc/self/cgroup to retrieve the paths to the memory controller (cgroupv1) and the cgroup node (cgroupv2)
	f, err := os.Open(procSelfCgroupPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	cgroupControllersPaths := parseCgroupNodePath(f)

	// Retrieve the cgroup inode from /sys/fs/cgroup + controller + cgroupNodePath
	for _, controller := range []string{cgroupV1BaseController, ""} {
		cgroupNodePath, ok := cgroupControllersPaths[controller]
		if !ok {
			continue
		}
		inode := inodeForPath(path.Join(cgroupMountPath, controller, cgroupNodePath))
		if inode != "" {
			return inode
		}
	}
	return ""
}

// inodeForPath returns the inode for the provided path or empty on failure.
func inodeForPath(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	stats, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("in-%d", stats.Ino)
}

// readEntityID attempts to return the cgroup node inode or empty on failure.
func readEntityID(mountPath, cgroupPath string, isHostCgroupNamespace bool) string {
	// First try to emit the containerID if available. It will be retrieved if the container is
	// running in the host cgroup namespace, independently of the cgroup version.
	if containerID != "" {
		return "cid-" + containerID
	}
	// Rely on the inode if we're not running in the host cgroup namespace.
	if isHostCgroupNamespace {
		return ""
	}
	return getCgroupInode(mountPath, cgroupPath)
}

// EntityID attempts to return the container ID or the cgroup node controller's inode if the container ID
// is not available. The cid is prefixed with `cid-` and the inode with `in-`.
func EntityID() string {
	return entityID
}

// isHostCgroupNamespace checks if the agent is running in the host cgroup namespace.
func isHostCgroupNamespace() bool {
	fi, err := os.Stat("/proc/self/ns/cgroup")
	if err != nil {
		return false
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if ok {
		return stat.Ino == hostCgroupNamespaceInode
	}

	return false
}
