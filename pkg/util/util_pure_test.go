package util

import (
	"strings"
	"testing"
)

func TestParseDirMapping(t *testing.T) {
	// Use a real directory that exists on every OS (avoid hardcoding /tmp,
	// which does not exist on Windows).
	tmpDir := t.TempDir()
	cases := []struct {
		input      string
		wantLocal  string
		wantRemote string
		wantErr    bool
	}{
		{tmpDir + ":/remote", tmpDir, "/remote", false},
		{"no-separator", "", "", true},
		{"", "", "", true},
		{"/nonexistent-path-xyz:/app", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			local, remote, err := ParseDirMapping(c.input)
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseDirMapping(%q): err=%v, wantErr=%v", c.input, err, c.wantErr)
			}
			if !c.wantErr {
				if local != c.wantLocal {
					t.Fatalf("local: want %q, got %q", c.wantLocal, local)
				}
				if remote != c.wantRemote {
					t.Fatalf("remote: want %q, got %q", c.wantRemote, remote)
				}
			}
		})
	}
}

func TestConvertUIDToWorkload(t *testing.T) {
	cases := []struct{ uid, want string }{
		{"deployments.apps.productpage", "deployments.apps/productpage"},
		{"statefulsets.apps.redis", "statefulsets.apps/redis"},
		{"pods.my-pod", "pods/my-pod"},
	}
	for _, c := range cases {
		if got := ConvertUIDToWorkload(c.uid); got != c.want {
			t.Errorf("ConvertUIDToWorkload(%q) = %q, want %q", c.uid, got, c.want)
		}
	}
}

func TestConvertWorkloadToUID(t *testing.T) {
	cases := []struct{ workload, want string }{
		{"deployments.apps/productpage", "deployments.apps.productpage"},
		{"statefulsets.apps/redis", "statefulsets.apps.redis"},
		{"pods/my-pod", "pods.my-pod"},
	}
	for _, c := range cases {
		if got := ConvertWorkloadToUID(c.workload); got != c.want {
			t.Errorf("ConvertWorkloadToUID(%q) = %q, want %q", c.workload, got, c.want)
		}
	}
}

func TestConvertUIDRoundTrip(t *testing.T) {
	workload := "deployments.apps/productpage"
	uid := ConvertWorkloadToUID(workload)
	back := ConvertUIDToWorkload(uid)
	if back != workload {
		t.Errorf("round-trip failed: %q -> %q -> %q", workload, uid, back)
	}
}

func TestMerge(t *testing.T) {
	t.Run("merges two maps", func(t *testing.T) {
		from := map[string]int{"a": 1, "b": 2}
		to := map[string]int{"c": 3}
		result := Merge(from, to)
		if result["a"] != 1 || result["b"] != 2 || result["c"] != 3 {
			t.Errorf("unexpected merge result: %v", result)
		}
	})
	t.Run("to overwrites from on conflict", func(t *testing.T) {
		from := map[string]int{"a": 10}
		to := map[string]int{"a": 1}
		result := Merge(from, to)
		if result["a"] != 1 {
			t.Errorf("expected to to overwrite from: got %d", result["a"])
		}
	})
	t.Run("nil inputs", func(t *testing.T) {
		result := Merge[string, int](nil, nil)
		if len(result) != 0 {
			t.Errorf("expected empty map, got %v", result)
		}
	})
}

func TestIf(t *testing.T) {
	if got := If(true, "yes", "no"); got != "yes" {
		t.Errorf("If(true) = %q, want yes", got)
	}
	if got := If(false, "yes", "no"); got != "no" {
		t.Errorf("If(false) = %q, want no", got)
	}
	if got := If(true, 42, 0); got != 42 {
		t.Errorf("If[int](true) = %d, want 42", got)
	}
}

func TestFormatBanner_ContainsSlogan(t *testing.T) {
	result := FormatBanner("test banner")
	if result == "" {
		t.Error("FormatBanner should return non-empty string")
	}
	if !strings.Contains(result, "test banner") {
		t.Errorf("FormatBanner should contain the slogan, got: %s", result)
	}
}
