package run

import (
	"testing"

	"github.com/docker/go-connections/nat"
	corev1 "k8s.io/api/core/v1"
)

func TestConvertToStandardNotation(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr bool
	}{
		{
			name:  "standard port stays as-is",
			input: []string{"80:80/tcp"},
			want:  []string{"80:80/tcp"},
		},
		{
			name:  "key=value format converted",
			input: []string{"published=80,target=80,protocol=tcp"},
			want:  []string{"80:80/tcp"},
		},
		{
			name:  "key=value default protocol is tcp",
			input: []string{"published=8080,target=80"},
			want:  []string{"8080:80/tcp"},
		},
		{
			name:  "key=value with udp protocol",
			input: []string{"published=53,target=53,protocol=udp"},
			want:  []string{"53:53/udp"},
		},
		{
			name:    "invalid format with leading equals",
			input:   []string{"=bad"},
			wantErr: true,
		},
		{
			name:    "invalid format empty key",
			input:   []string{"published=80,=bad"},
			wantErr: true,
		},
		{
			name:  "multiple entries mixed formats",
			input: []string{"80:80/tcp", "published=3000,target=3000,protocol=tcp"},
			want:  []string{"80:80/tcp", "3000:3000/tcp"},
		},
		{
			name:  "empty input",
			input: []string{},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertToStandardNotation(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("convertToStandardNotation() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("convertToStandardNotation() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("convertToStandardNotation()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidateAttach(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "stdin lowercase", input: "stdin", want: "stdin"},
		{name: "stdout lowercase", input: "stdout", want: "stdout"},
		{name: "stderr lowercase", input: "stderr", want: "stderr"},
		{name: "STDIN uppercase", input: "STDIN", want: "stdin"},
		{name: "STDOUT uppercase", input: "STDOUT", want: "stdout"},
		{name: "Stderr mixed case", input: "Stderr", want: "stderr"},
		{name: "invalid value", input: "other", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateAttach(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAttach(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("validateAttach(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConvertK8sImagePullPolicyToDocker(t *testing.T) {
	tests := []struct {
		name   string
		policy corev1.PullPolicy
		want   string
	}{
		{name: "PullAlways", policy: corev1.PullAlways, want: "always"},
		{name: "PullNever", policy: corev1.PullNever, want: "never"},
		{name: "PullIfNotPresent", policy: corev1.PullIfNotPresent, want: "missing"},
		{name: "empty default", policy: "", want: "missing"},
		{name: "unknown value", policy: "SomethingElse", want: "missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertK8sImagePullPolicyToDocker(tt.policy)
			if got != tt.want {
				t.Errorf("ConvertK8sImagePullPolicyToDocker(%q) = %q, want %q", tt.policy, got, tt.want)
			}
		})
	}
}

func TestConvertToDockerArgs(t *testing.T) {
	t.Run("with command and args", func(t *testing.T) {
		rc := &RunConfig{
			options: []string{"--rm", "--privileged"},
			command: []string{"/bin/sh", "-c"},
			image:   "nginx:latest",
			args:    []string{"echo", "hello"},
		}
		got := convertToDockerArgs(rc)
		expected := []string{"docker", "run", "--rm", "--privileged", "--entrypoint", "/bin/sh -c", "nginx:latest", "echo", "hello"}

		if len(got) != len(expected) {
			t.Fatalf("convertToDockerArgs() = %v, want %v", got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Errorf("convertToDockerArgs()[%d] = %q, want %q", i, got[i], expected[i])
			}
		}
	})

	t.Run("without command", func(t *testing.T) {
		rc := &RunConfig{
			options: []string{"--tty"},
			image:   "alpine",
			args:    []string{"sleep", "60"},
		}
		got := convertToDockerArgs(rc)
		expected := []string{"docker", "run", "--tty", "alpine", "sleep", "60"}

		if len(got) != len(expected) {
			t.Fatalf("convertToDockerArgs() = %v, want %v", got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Errorf("convertToDockerArgs()[%d] = %q, want %q", i, got[i], expected[i])
			}
		}
	})

	t.Run("minimal config", func(t *testing.T) {
		rc := &RunConfig{image: "busybox"}
		got := convertToDockerArgs(rc)
		expected := []string{"docker", "run", "busybox"}

		if len(got) != len(expected) {
			t.Fatalf("convertToDockerArgs() = %v, want %v", got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Errorf("convertToDockerArgs()[%d] = %q, want %q", i, got[i], expected[i])
			}
		}
	})
}

func TestGetEnvByKey(t *testing.T) {
	t.Run("nonexistent file returns default", func(t *testing.T) {
		got := getEnvByKey("/nonexistent/path/env.file", "KEY", "default-value")
		if got != "default-value" {
			t.Errorf("getEnvByKey() = %q, want %q", got, "default-value")
		}
	})
}

func TestMoveDevContainerFirst(t *testing.T) {
	t.Run("moves target to index 0", func(t *testing.T) {
		spec := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "sidecar"},
					{Name: "app"},
					{Name: "logger"},
				},
			},
		}
		moveDevContainerFirst(spec, "app")
		if spec.Spec.Containers[0].Name != "app" {
			t.Errorf("expected app at index 0, got %s", spec.Spec.Containers[0].Name)
		}
		if spec.Spec.Containers[1].Name != "sidecar" {
			t.Errorf("expected sidecar at index 1, got %s", spec.Spec.Containers[1].Name)
		}
	})

	t.Run("already first is no-op", func(t *testing.T) {
		spec := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app"},
					{Name: "sidecar"},
				},
			},
		}
		moveDevContainerFirst(spec, "app")
		if spec.Spec.Containers[0].Name != "app" {
			t.Errorf("expected app at index 0, got %s", spec.Spec.Containers[0].Name)
		}
	})

	t.Run("not found is no-op", func(t *testing.T) {
		spec := &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "a"},
					{Name: "b"},
				},
			},
		}
		moveDevContainerFirst(spec, "nonexistent")
		if spec.Spec.Containers[0].Name != "a" {
			t.Errorf("expected a at index 0, got %s", spec.Spec.Containers[0].Name)
		}
	})
}

func TestCollectExposedPorts(t *testing.T) {
	t.Run("merges config and host ports", func(t *testing.T) {
		conf := &Config{
			ExposedPorts: map[nat.Port]struct{}{
				"80/tcp": {},
			},
		}
		hc := &HostConfig{
			PortBindings: map[nat.Port][]nat.PortBinding{
				"8080/tcp": {{HostPort: "8080"}},
			},
		}
		portMap, portSet := collectExposedPorts(conf, hc)
		if _, ok := portSet["80/tcp"]; !ok {
			t.Error("expected 80/tcp in portSet")
		}
		if _, ok := portMap["8080/tcp"]; !ok {
			t.Error("expected 8080/tcp in portMap")
		}
	})

	t.Run("empty inputs", func(t *testing.T) {
		conf := &Config{}
		hc := &HostConfig{}
		portMap, portSet := collectExposedPorts(conf, hc)
		if len(portMap) != 0 || len(portSet) != 0 {
			t.Error("expected empty results for empty inputs")
		}
	})
}
