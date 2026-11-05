package util

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestFindContainerByName(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.25"},
				{Name: "sidecar", Image: "envoy:latest"},
				{Name: "debug", Image: "busybox"},
			},
		},
	}

	container, idx := FindContainerByName(pod, "sidecar")
	if container == nil {
		t.Fatal("expected to find container 'sidecar', got nil")
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
	if container.Image != "envoy:latest" {
		t.Fatalf("expected image 'envoy:latest', got %q", container.Image)
	}
}

func TestFindContainerByName_First(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.25"},
				{Name: "sidecar", Image: "envoy:latest"},
			},
		},
	}

	container, idx := FindContainerByName(pod, "app")
	if container == nil {
		t.Fatal("expected to find container 'app', got nil")
	}
	if idx != 0 {
		t.Fatalf("expected index 0, got %d", idx)
	}
}

func TestFindContainerByName_NotFound(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.25"},
			},
		},
	}

	container, idx := FindContainerByName(pod, "nonexistent")
	if container != nil {
		t.Fatalf("expected nil container, got %+v", container)
	}
	if idx != -1 {
		t.Fatalf("expected index -1, got %d", idx)
	}
}

func TestFindContainerEnv(t *testing.T) {
	container := &corev1.Container{
		Name: "app",
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/root"},
			{Name: "PORT", Value: "8080"},
			{Name: "DEBUG", Value: "true"},
		},
	}

	value, found := FindContainerEnv(container, "PORT")
	if !found {
		t.Fatal("expected to find env var 'PORT'")
	}
	if value != "8080" {
		t.Fatalf("expected value '8080', got %q", value)
	}
}

func TestFindContainerEnv_NotFound(t *testing.T) {
	container := &corev1.Container{
		Name: "app",
		Env: []corev1.EnvVar{
			{Name: "HOME", Value: "/root"},
		},
	}

	value, found := FindContainerEnv(container, "NONEXISTENT")
	if found {
		t.Fatal("expected found=false for nonexistent env var")
	}
	if value != "" {
		t.Fatalf("expected empty value, got %q", value)
	}
}

func TestFindContainerEnv_NilContainer(t *testing.T) {
	value, found := FindContainerEnv(nil, "KEY")
	if found {
		t.Fatal("expected found=false for nil container")
	}
	if value != "" {
		t.Fatalf("expected empty value, got %q", value)
	}
}

func TestFindContainerEnv_EmptyEnv(t *testing.T) {
	container := &corev1.Container{
		Name: "app",
		Env:  []corev1.EnvVar{},
	}

	value, found := FindContainerEnv(container, "KEY")
	if found {
		t.Fatal("expected found=false for empty env list")
	}
	if value != "" {
		t.Fatalf("expected empty value, got %q", value)
	}
}
