package util

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAllContainersRunning(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true},
				{Name: "sidecar", Ready: true},
			},
		},
	}
	if !AllContainersRunning(pod) {
		t.Fatal("expected AllContainersRunning to return true when all containers are ready")
	}
}

func TestAllContainersRunning_NotReady(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true},
				{Name: "sidecar", Ready: false},
			},
		},
	}
	if AllContainersRunning(pod) {
		t.Fatal("expected AllContainersRunning to return false when one container is not ready")
	}
}

func TestAllContainersRunning_PodNotReady(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true},
			},
		},
	}
	if AllContainersRunning(pod) {
		t.Fatal("expected AllContainersRunning to return false when pod condition is not ready")
	}
}

func TestAllContainersRunning_Empty(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{},
		},
	}
	if !AllContainersRunning(pod) {
		t.Fatal("expected AllContainersRunning to return true for pod with no containers (vacuous truth)")
	}
}

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
