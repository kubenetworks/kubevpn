package inject

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func testAgentJobInput(pod *v1.Pod) AgentJobInput {
	return AgentJobInput{
		ManagerNamespace: "kubevpn",
		Namespace:        "default",
		NodeID:           "deployments.apps.reviews",
		Image:            config.Image,
		OwnerID:          "owner-1",
		Pod:              pod,
	}
}

func readyAgentPod(jobName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abc",
			Namespace: "kubevpn",
			Labels:    map[string]string{"job-name": jobName},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "vpn"}, {Name: "envoy"}},
		},
		Status: v1.PodStatus{
			Phase:             v1.PodRunning,
			ContainerStatuses: []v1.ContainerStatus{{Name: "vpn", Ready: true}, {Name: "envoy", Ready: true}},
		},
	}
}

func pendingAgentPod(jobName string) *v1.Pod {
	p := readyAgentPod(jobName)
	p.Status.Phase = v1.PodPending
	p.Status.ContainerStatuses = nil
	return p
}

func shortenTimeouts(t *testing.T, ready, terminating time.Duration) {
	t.Helper()
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = ready, terminating
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})
}

func TestEnsureAgentJob_ReturnsWhenPodReady(t *testing.T) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-1"}}
	jobName := AgentJobName(pod.UID)
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job, readyAgentPod(jobName))

	if err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod)); err != nil {
		t.Fatalf("EnsureAgentJob: %v", err)
	}
}

func TestEnsureAgentJob_TimesOutWhenPodNeverReady(t *testing.T) {
	shortenTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-2"}}
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job, pendingAgentPod(AgentJobName(pod.UID)))

	err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod))
	if err == nil || !strings.Contains(err.Error(), "not ready within") {
		t.Fatalf("expected not-ready timeout, got %v", err)
	}
}

func TestEnsureAgentJob_WaitsTerminatingThenRecreates(t *testing.T) {
	shortenTimeouts(t, time.Second, 2*time.Second)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "reviews-x", Namespace: "default", UID: "uid-3"}}
	jobName := AgentJobName(pod.UID)
	job := GenAgentJobSpec(testAgentJobInput(pod))
	clientset := fake.NewSimpleClientset(job)

	// Simulate the job disappearing and its replacement pod becoming ready.
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Fake clientset doesn't respect DeletionTimestamp; just delete the job directly
		_ = clientset.BatchV1().Jobs("kubevpn").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		// Create the new job and ready pod
		_, _ = clientset.BatchV1().Jobs("kubevpn").Create(context.Background(), GenAgentJobSpec(testAgentJobInput(pod)), metav1.CreateOptions{})
		_, _ = clientset.CoreV1().Pods("kubevpn").Create(context.Background(), readyAgentPod(AgentJobName(pod.UID)), metav1.CreateOptions{})
	}()

	if err := EnsureAgentJob(context.Background(), clientset, testAgentJobInput(pod)); err != nil {
		t.Fatalf("EnsureAgentJob should recreate after terminating job disappears: %v", err)
	}
}
