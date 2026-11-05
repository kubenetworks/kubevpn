package workload

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodStatusSummary(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "container waiting reasons",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "xds", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
				{Name: "vpn", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
			}}},
			want: "xds=ContainerCreating, vpn=ContainerCreating",
		},
		{
			name: "running container",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				{Name: "vpn", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}}},
			want: "vpn=Running",
		},
		{
			name: "unschedulable condition (no container statuses)",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available: 1 Insufficient memory."},
			}}},
			want: "PodScheduled=Unschedulable",
		},
		{
			name: "phase fallback",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: "Pending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PodStatusSummary(tt.pod); got != tt.want {
				t.Errorf("PodStatusSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

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
