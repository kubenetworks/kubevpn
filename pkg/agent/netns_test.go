package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// buildFakeProc fabricates a /proc tree: numeric dirs with cgroup files.
func buildFakeProc(t *testing.T, entries map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, cg := range entries {
		dir := filepath.Join(root, itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestFindNetnsPIDMatchesPodUIDInCgroupV1AndV2(t *testing.T) {
	uid := "6f2a-4c1e-9a0b"
	root := buildFakeProc(t, map[int]string{
		1:    "12:cpuset:/docker/other",
		3482: "0::/kubepods/pod" + uid + "/9be2a1f...",
		3501: "10:cpu,cpuacct:/kubepods/burstable/pod" + uid + "/cc11...",
	})
	pid, err := FindNetnsPID(root, uid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != 3482 {
		t.Fatalf("want lowest matching PID 3482, got %d", pid)
	}
}

func TestFindNetnsPIDErrorsWhenAbsent(t *testing.T) {
	root := buildFakeProc(t, map[int]string{1: "0::/"})
	if _, err := FindNetnsPID(root, "nope"); err == nil {
		t.Fatal("expected error when no process matches")
	}
}
