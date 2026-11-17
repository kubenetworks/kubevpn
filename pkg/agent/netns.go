// Package agent implements the node-hosted traffic agent (docs/55): locating a
// target pod's network namespace from outside the pod and entering it.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FindNetnsPID scans procRoot for the first process whose cgroup path contains
// podUID (cgroup v1 and v2 both embed the pod UID in the kubelet's cgroupfs
// layout: .../kubepods/pod<pod-uid>/...). procRoot is the mounted host /proc.
func FindNetnsPID(procRoot, podUID string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("read proc root %s: %w", procRoot, err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // skip non-numeric entries (cgroups, mounts, ...)
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cgroup"))
		if err != nil {
			continue // kernel threads / racing exits
		}
		if strings.Contains(string(data), podUID) {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("cannot find netns for pod uid %s under %s", podUID, procRoot)
}
