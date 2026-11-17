package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// buildFakeProc fabricates a /proc tree: numeric dirs with cgroup and comm files.
func buildFakeProc(t *testing.T, entries map[int]procEntry) string {
	t.Helper()
	root := t.TempDir()
	for pid, entry := range entries {
		dir := filepath.Join(root, itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(entry.cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
		if entry.comm != "" {
			if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(entry.comm+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

type procEntry struct {
	cgroup string
	comm   string
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestFindNetnsPIDMatchesPodUIDInCgroupV1AndV2(t *testing.T) {
	uid := "6f2a-4c1e-9a0b"
	root := buildFakeProc(t, map[int]procEntry{
		1:    {cgroup: "12:cpuset:/docker/other"},
		3482: {cgroup: "0::/kubepods/pod" + uid + "/9be2a1f...", comm: "pause"},
		3501: {cgroup: "10:cpu,cpuacct:/kubepods/burstable/pod" + uid + "/cc11...", comm: "nginx"},
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
	root := buildFakeProc(t, map[int]procEntry{1: {cgroup: "0::/"}})
	if _, err := FindNetnsPID(root, "nope"); err == nil {
		t.Fatal("expected error when no process matches")
	}
}

func TestFindNetnsPIDSkipsShimProcesses(t *testing.T) {
	uid := "pod-1234-5678"
	root := buildFakeProc(t, map[int]procEntry{
		100: {cgroup: "0::/kubepods/pod" + uid + "/abc", comm: "containerd-shim"}, // lower PID but shim
		101: {cgroup: "0::/kubepods/pod" + uid + "/def", comm: "conmon"},          // another shim
		200: {cgroup: "0::/kubepods/pod" + uid + "/ghi", comm: "pause"},           // valid, smallest valid PID
		201: {cgroup: "0::/kubepods/pod" + uid + "/jkl", comm: "nginx"},           // valid but higher PID
	})
	pid, err := FindNetnsPID(root, uid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != 200 {
		t.Fatalf("want pause PID 200 (skipping shims 100/101), got %d", pid)
	}
}

func TestFindNetnsPIDErrorsWhenOnlyShims(t *testing.T) {
	uid := "pod-9876-5432"
	root := buildFakeProc(t, map[int]procEntry{
		100: {cgroup: "0::/kubepods/pod" + uid + "/abc", comm: "containerd-shim"},
		101: {cgroup: "0::/kubepods/pod" + uid + "/def", comm: "dockerd"},
	})
	_, err := FindNetnsPID(root, uid)
	if err == nil {
		t.Fatal("expected error when only shim processes found")
	}
	if !strContains(err.Error(), "only shim processes found") {
		t.Fatalf("error should mention only shims, got: %v", err)
	}
}

func TestFindNetnsPIDMultipleNonShimSelectsSmallest(t *testing.T) {
	uid := "pod-aaa-bbb"
	root := buildFakeProc(t, map[int]procEntry{
		300: {cgroup: "0::/kubepods/pod" + uid + "/x", comm: "sidecar"},
		250: {cgroup: "0::/kubepods/pod" + uid + "/y", comm: "pause"},
		275: {cgroup: "0::/kubepods/pod" + uid + "/z", comm: "app"},
	})
	pid, err := FindNetnsPID(root, uid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != 250 {
		t.Fatalf("want smallest non-shim PID 250, got %d", pid)
	}
}

func TestFindNetnsPIDHandlesMissingCommFile(t *testing.T) {
	uid := "pod-missing-comm"
	root := t.TempDir()
	// Build manually since buildFakeProc always writes comm if present
	pid100Dir := filepath.Join(root, "100")
	_ = os.MkdirAll(pid100Dir, 0o755)
	_ = os.WriteFile(filepath.Join(pid100Dir, "cgroup"), []byte("0::/kubepods/pod"+uid+"/abc"), 0o644)
	// No comm file for pid 100

	pid200Dir := filepath.Join(root, "200")
	_ = os.MkdirAll(pid200Dir, 0o755)
	_ = os.WriteFile(filepath.Join(pid200Dir, "cgroup"), []byte("0::/kubepods/pod"+uid+"/def"), 0o644)
	_ = os.WriteFile(filepath.Join(pid200Dir, "comm"), []byte("pause\n"), 0o644)

	pid, err := FindNetnsPID(root, uid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// pid 100 has no comm, so it's considered valid and smaller than 200
	if pid != 100 {
		t.Fatalf("want PID 100 (missing comm treated as valid), got %d", pid)
	}
}

func TestFindNetnsPIDAllKnownShimsFiltered(t *testing.T) {
	uid := "pod-shim-filter"
	root := buildFakeProc(t, map[int]procEntry{
		100: {cgroup: "0::/kubepods/pod" + uid + "/a", comm: "containerd-shim"},
		101: {cgroup: "0::/kubepods/pod" + uid + "/b", comm: "conmon"},
		102: {cgroup: "0::/kubepods/pod" + uid + "/c", comm: "containerd"},
		103: {cgroup: "0::/kubepods/pod" + uid + "/d", comm: "dockerd"},
		104: {cgroup: "0::/kubepods/pod" + uid + "/e", comm: "kubelet"},
		200: {cgroup: "0::/kubepods/pod" + uid + "/f", comm: "pause"},
	})
	pid, err := FindNetnsPID(root, uid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != 200 {
		t.Fatalf("want pause PID 200 after filtering all shims, got %d", pid)
	}
}

func strContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) && (s[:len(substr)] == substr ||
			s[len(s)-len(substr):] == substr ||
			containsMiddle(s, substr))))
}

func containsMiddle(s, substr string) bool {
	for i := 1; i < len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
