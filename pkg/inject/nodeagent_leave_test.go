package inject

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUnpatchContainerDeletesAgentJobsOnEmpty(t *testing.T) {
	cs, opts, pod := newFakeWorld(t) // same helper as Task 4
	// Seed the pod into the fake clientset so listControllerPods finds it
	_, err := cs.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Shorten timeouts for test
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})

	// Start goroutine to create ready pod
	jobName := AgentJobName(pod.UID)
	go func() {
		time.Sleep(100 * time.Millisecond)
		createReadyAgentPod(t, cs, opts.ManagerNamespace, jobName)
	}()

	_ = NewInjector(opts).Inject(context.Background())
	// owner-1 leaves: remove its rule → job must be deleted
	_, err = UnpatchContainer(context.Background(), opts.NodeID, opts.Factory,
		cs.CoreV1().ConfigMaps(opts.ManagerNamespace), opts.Controller, opts.OwnerID,
		cs, opts.ManagerNamespace)
	if err != nil {
		t.Fatalf("unpatch: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs(opts.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 0 {
		t.Fatalf("agent job must be deleted when the last rule is removed, got %d", len(jobs.Items))
	}
}

func TestUnpatchContainerKeepsJobsWhileOtherOwnersRemain(t *testing.T) {
	cs, opts, pod := newFakeWorld(t)
	// Seed the pod into the fake clientset so listControllerPods finds it
	_, err := cs.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Shorten timeouts for test
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})

	// Start goroutine to create ready pod
	jobName := AgentJobName(pod.UID)
	go func() {
		time.Sleep(100 * time.Millisecond)
		createReadyAgentPod(t, cs, opts.ManagerNamespace, jobName)
	}()

	_ = NewInjector(opts).Inject(context.Background())
	opts2 := opts
	opts2.OwnerID = "owner-2"
	opts2.Headers = map[string]string{"X-User": "b"}

	// Start another goroutine for the second inject (in case it needs the pod too)
	go func() {
		time.Sleep(100 * time.Millisecond)
		createReadyAgentPod(t, cs, opts.ManagerNamespace, jobName)
	}()

	_ = NewInjector(opts2).Inject(context.Background())
	_, err = UnpatchContainer(context.Background(), opts.NodeID, opts.Factory,
		cs.CoreV1().ConfigMaps(opts.ManagerNamespace), opts.Controller, opts.OwnerID,
		cs, opts.ManagerNamespace)
	if err != nil {
		t.Fatalf("unpatch: %v", err)
	}
	jobs, _ := cs.BatchV1().Jobs(opts.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("job must survive while owner-2 still holds rules, got %d", len(jobs.Items))
	}
}
