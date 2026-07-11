package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func TestJoin(t *testing.T) {
	cases := []struct {
		name  string
		input []string
		want  string
	}{
		{"single", []string{"foo"}, "foo"},
		{"two", []string{"foo", "bar"}, "foo_bar"},
		{"three", []string{"a", "b", "c"}, "a_b_c"},
		{"empty_strings", []string{"", ""}, "_"},
		{"no_args", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Join(c.input...)
			if got != c.want {
				t.Fatalf("Join(%v): want %q, got %q", c.input, c.want, got)
			}
		})
	}
}

func TestContainerNet(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"mycontainer", "container:mycontainer"},
		{"abc-123", "container:abc-123"},
		{"", "container:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ContainerNet(c.name)
			if got != c.want {
				t.Fatalf("ContainerNet(%q): want %q, got %q", c.name, c.want, got)
			}
		})
	}
}

func TestGenEnvoyUID(t *testing.T) {
	cases := []struct {
		testName string
		ns       string
		uid      string
		want     string
	}{
		{"normal", "default", "abc123", "default.abc123"},
		{"empty_ns", "", "uid", ".uid"},
		{"empty_uid", "ns", "", "ns."},
		{"both_empty", "", "", "."},
	}
	for _, c := range cases {
		t.Run(c.testName, func(t *testing.T) {
			got := GenEnvoyUID(c.ns, c.uid)
			if got != c.want {
				t.Fatalf("GenEnvoyUID(%q, %q): want %q, got %q", c.ns, c.uid, c.want, got)
			}
		})
	}
}

func TestContainsPathSeparator(t *testing.T) {
	sep := string(os.PathSeparator)
	cases := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"no_separator", "hello", false},
		{"empty", "", false},
		{"has_separator", "path" + sep + "file", true},
		{"only_separator", sep, true},
		{"leading_separator", sep + "usr", true},
		{"trailing_separator", "usr" + sep, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ContainsPathSeparator(c.pattern)
			if got != c.want {
				t.Fatalf("ContainsPathSeparator(%q): want %v, got %v", c.pattern, c.want, got)
			}
		})
	}
}

func TestGenKubeconfigTempPath(t *testing.T) {
	t.Run("invalid_kubeconfig_falls_back_to_timestamp", func(t *testing.T) {
		path := GenKubeconfigTempPath([]byte("not-valid-kubeconfig"))
		if !strings.HasPrefix(path, config.GetTempPath()) {
			t.Fatalf("expected prefix %q, got %q", config.GetTempPath(), path)
		}
		base := filepath.Base(path)
		// With invalid kubeconfig, GetCluster returns empty strings (no path separators),
		// so the path format is "<cluster>_<ns>_<timestamp>" where cluster and ns are empty.
		if !strings.Contains(base, "_") {
			t.Fatalf("expected underscore-separated base, got %q", base)
		}
	})

	t.Run("returns_under_temp_path", func(t *testing.T) {
		path := GenKubeconfigTempPath(nil)
		if !strings.HasPrefix(path, config.GetTempPath()) {
			t.Fatalf("expected prefix %q, got %q", config.GetTempPath(), path)
		}
	})

	t.Run("different_calls_produce_same_prefix", func(t *testing.T) {
		p1 := GenKubeconfigTempPath(nil)
		p2 := GenKubeconfigTempPath(nil)
		dir1 := filepath.Dir(p1)
		dir2 := filepath.Dir(p2)
		if dir1 != dir2 {
			t.Fatalf("directories differ: %q vs %q", dir1, dir2)
		}
	})
}
