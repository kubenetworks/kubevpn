package util

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// makeKubeconfig builds a minimal kubeconfig YAML with the given server URL.
func makeKubeconfig(server string) []byte {
	return []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + server + `
    insecure-skip-tls-verify: true
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: fake
`)
}

func TestGetAPIServerFromKubeConfigBytes(t *testing.T) {
	tests := []struct {
		name   string
		server string
		want   string // expected IPNet.String(), or "" for nil
	}{
		{
			name:   "IPv4 with port",
			server: "https://10.0.0.1:6443",
			want:   "10.0.0.1/32",
		},
		{
			name:   "IPv4 without port",
			server: "https://192.168.1.100:443",
			want:   "192.168.1.100/32",
		},
		{
			name:   "IPv6 with port",
			server: "https://[fd00::1]:6443",
			want:   "fd00::1/128",
		},
		{
			name:   "hostname returns nil",
			server: "https://kube.example.com:6443",
			want:   "",
		},
		{
			name:   "hostname without port returns nil",
			server: "https://kube.example.com",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetAPIServerFromKubeConfigBytes(makeKubeconfig(tt.server))
			if tt.want == "" {
				if got != nil {
					t.Fatalf("expected nil, got %s", got.String())
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %s, got nil", tt.want)
			}
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got.String(), tt.want)
			}
		})
	}
}

func TestGetAPIServerFromKubeConfigBytes_InvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "nil bytes",
			input: nil,
		},
		{
			name:  "empty bytes",
			input: []byte{},
		},
		{
			name:  "invalid YAML",
			input: []byte("not valid kubeconfig"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetAPIServerFromKubeConfigBytes(tt.input)
			if got != nil {
				t.Fatalf("expected nil for invalid input, got %s", got.String())
			}
		})
	}
}

func TestGetAPIServerFromKubeConfigBytes_IPv4Mask(t *testing.T) {
	got := GetAPIServerFromKubeConfigBytes(makeKubeconfig("https://172.16.0.5:6443"))
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	expectedMask := net.CIDRMask(32, 32)
	if got.Mask.String() != expectedMask.String() {
		t.Errorf("IPv4 mask: got %s, want %s", got.Mask.String(), expectedMask.String())
	}
}

func TestGetAPIServerFromKubeConfigBytes_IPv6Mask(t *testing.T) {
	got := GetAPIServerFromKubeConfigBytes(makeKubeconfig("https://[2001:db8::1]:6443"))
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	expectedMask := net.CIDRMask(128, 128)
	if got.Mask.String() != expectedMask.String() {
		t.Errorf("IPv6 mask: got %s, want %s", got.Mask.String(), expectedMask.String())
	}
}

func TestConvertToTempKubeconfigFile(t *testing.T) {
	content := makeKubeconfig("https://10.0.0.1:6443")

	t.Run("explicit path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kubeconfig-test")
		got, err := ConvertToTempKubeconfigFile(content, path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != path {
			t.Errorf("returned path %q, want %q", got, path)
		}

		data, err := os.ReadFile(got)
		if err != nil {
			t.Fatalf("failed to read written file: %v", err)
		}
		if string(data) != string(content) {
			t.Error("file content does not match input")
		}

		info, err := os.Stat(got)
		if err != nil {
			t.Fatalf("failed to stat file: %v", err)
		}
		// Windows does not honor Unix file permission bits; Stat always
		// reports 0666 there regardless of the Chmod(0644) call.
		if runtime.GOOS != "windows" {
			perm := info.Mode().Perm()
			if perm != 0644 {
				t.Errorf("file permissions: got %o, want 0644", perm)
			}
		}
	})

	t.Run("auto-generated path", func(t *testing.T) {
		got, err := ConvertToTempKubeconfigFile(content, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer os.Remove(got)

		if got == "" {
			t.Fatal("expected non-empty path")
		}

		data, err := os.ReadFile(got)
		if err != nil {
			t.Fatalf("failed to read written file: %v", err)
		}
		if string(data) != string(content) {
			t.Error("file content does not match input")
		}
	})

	t.Run("invalid path returns error", func(t *testing.T) {
		_, err := ConvertToTempKubeconfigFile(content, "/nonexistent-dir-abc123/kubeconfig")
		if err == nil {
			t.Fatal("expected error for invalid path, got nil")
		}
	})
}
