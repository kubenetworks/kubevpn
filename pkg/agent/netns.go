// Package agent implements the node-hosted traffic agent (docs/55): locating a
// target pod's network namespace from outside the pod and entering it.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var shimProcessNames = map[string]bool{
	"containerd-shim": true,
	"conmon":          true,
	"containerd":      true,
	"dockerd":         true,
	"kubelet":         true,
}

// FindNetnsPID scans procRoot for processes whose cgroup path contains
// podUID (cgroup v1 and v2 both embed the pod UID in the kubelet's cgroupfs
// layout: .../kubepods/pod<pod-uid>/...), filters out container runtime shim
// processes, and returns the smallest remaining PID (which is typically the
// pause/infra container that shares the pod's network namespace).
// procRoot is the mounted host /proc.
//
// Handles both cgroupfs and systemd cgroup drivers:
//   - cgroupfs driver uses raw pod UID with hyphens: "pod36a197bc-6510-443d-b926-fa0a09e91efd"
//   - systemd driver escapes hyphens to underscores: "pod36a197bc_6510_443d_b926_fa0a09e91efd"
//
// We normalize cgroup content by replacing underscores with hyphens before matching,
// which works for both drivers (cgroupfs paths are unaffected, systemd paths are
// converted back to raw UID form for matching).
func FindNetnsPID(procRoot, podUID string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("read proc root %s: %w", procRoot, err)
	}

	// Get self PID once to exclude ourselves (only if procRoot is real /proc)
	var selfPID int
	if procRoot == "/proc" {
		if selfPIDStr, err := os.Readlink("/proc/self"); err == nil {
			selfPID, _ = strconv.Atoi(selfPIDStr)
		}
	}

	// Collect all candidate PIDs whose cgroup contains podUID
	var candidates []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // skip non-numeric entries (cgroups, mounts, ...)
		}
		if pid == selfPID {
			continue // skip ourselves
		}
		data, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cgroup"))
		if err != nil {
			continue // kernel threads / racing exits
		}
		// Normalize cgroup content: replace underscores with hyphens to handle
		// systemd cgroup driver's escaping of pod UID hyphens to underscores
		normalized := strings.ReplaceAll(string(data), "_", "-")
		if strings.Contains(normalized, podUID) {
			candidates = append(candidates, pid)
		}
	}

	if len(candidates) == 0 {
		return 0, fmt.Errorf("cannot find netns for pod uid %s under %s", podUID, procRoot)
	}

	// Filter out shim processes by reading /proc/<pid>/comm
	var validPIDs []int
	for _, pid := range candidates {
		commBytes, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
		if err != nil {
			// If we can't read comm, assume it's a valid process (could be racing exit)
			validPIDs = append(validPIDs, pid)
			continue
		}
		comm := strings.TrimSpace(string(commBytes))
		if !shimProcessNames[comm] {
			validPIDs = append(validPIDs, pid)
		}
	}

	if len(validPIDs) == 0 {
		return 0, fmt.Errorf("only shim processes found for pod uid %s under %s (no valid netns candidates)", podUID, procRoot)
	}

	// Select the smallest PID (pause/infra container PID is typically lowest)
	sort.Ints(validPIDs)
	return validPIDs[0], nil
}
