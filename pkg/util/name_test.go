package util

import (
	"os"
	"strings"
	"testing"
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

func TestGenKubeconfigTempPattern(t *testing.T) {
	t.Run("ends_with_star_for_createtemp", func(t *testing.T) {
		// os.CreateTemp replaces the trailing "*" with a unique suffix.
		if got := GenKubeconfigTempPattern(nil); !strings.HasSuffix(got, "*") {
			t.Fatalf("pattern must end with *, got %q", got)
		}
	})

	t.Run("descriptive_cluster_ns_prefix", func(t *testing.T) {
		got := GenKubeconfigTempPattern(makeKubeconfig("https://10.0.0.1:6443"))
		// makeKubeconfig uses cluster "test" with no namespace → "test__*".
		if !strings.HasPrefix(got, "test_") || !strings.HasSuffix(got, "*") {
			t.Fatalf("expected 'test_..._*' pattern, got %q", got)
		}
	})

	t.Run("path_separator_falls_back_to_safe_pattern", func(t *testing.T) {
		if got := GenKubeconfigTempPattern([]byte("not-valid-kubeconfig")); strings.ContainsRune(got, os.PathSeparator) {
			t.Fatalf("pattern must not contain a path separator, got %q", got)
		}
	})
}
