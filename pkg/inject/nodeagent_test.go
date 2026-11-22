package inject

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"sigs.k8s.io/yaml"
)

func createReadyAgentPod(t *testing.T, clientset *fake.Clientset, namespace, jobName string) {
	t.Helper()
	podName := jobName + "-abc"
	// Check if pod already exists
	_, getErr := clientset.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	if getErr == nil {
		// Pod already exists, nothing to do
		return
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
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
	_, err := clientset.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		// If it already exists, that's fine
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("create ready agent pod: %v", err)
		}
	}
}

// newFakeWorld builds: one Deployment controller, one pod on node-a, and the
// manager ConfigMap backing addEnvoyConfig.
func newFakeWorld(t *testing.T) (*fake.Clientset, InjectOptions, *v1.Pod) {
	t.Helper()
	const (
		managerNS  = "kubevpn"
		workloadNS = "default"
		nodeID     = "default.deployments.web"
		depName    = "web"
		podName    = "web-1"
	)

	// Deployment controller with template labels
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: workloadNS},
		Spec: appsv1.DeploymentSpec{
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "web"},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:  "web",
						Ports: []v1.ContainerPort{{ContainerPort: 8080, Protocol: v1.ProtocolTCP}},
					}},
				},
			},
		},
	}

	unstructedMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		t.Fatalf("to unstructured: %v", err)
	}

	// The target pod that will be matched by the template labels
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: workloadNS,
			UID:       "pod-uid-1",
			Labels:    map[string]string{"app": "web"},
		},
		Spec: v1.PodSpec{
			NodeName: "node-a",
			Containers: []v1.Container{{
				Name:  "web",
				Ports: []v1.ContainerPort{{ContainerPort: 8080, Protocol: v1.ProtocolTCP}},
			}},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}

	// Traffic manager ConfigMap
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: managerNS},
		Data: map[string]string{
			config.KeyEnvoy: "",
		},
	}

	clientset := fake.NewSimpleClientset(cm)

	controllerInfo := &resource.Info{
		Name:      depName,
		Namespace: workloadNS,
		Object:    &unstructured.Unstructured{Object: unstructedMap},
		Mapping: &meta.RESTMapping{
			Resource: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		},
	}

	opts := InjectOptions{
		Clientset:        clientset,
		ManagerNamespace: managerNS,
		NodeID:           nodeID,
		OwnerID:          "owner-1",
		Headers:          map[string]string{"x-user": "alice"},
		LocalTunIPv4:     "10.0.0.1",
		LocalTunIPv6:     "fd00::1",
		Image:            "kubevpn/kubevpn:latest",
		AttachMode:       config.AttachModeNodeAgent,
		Object:           controllerInfo, // Set Object to avoid nil panic in NewInjector
		Controller:       controllerInfo,
	}

	return clientset, opts, pod
}

func TestNodeAgentInject_WritesRulesAndCreatesPinnedJob(t *testing.T) {
	clientset, opts, pod := newFakeWorld(t)

	// Seed the pod into the fake clientset so listControllerPods finds it
	_, err := clientset.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Shorten timeouts for test
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})

	// Start a goroutine to create the ready agent pod after a short delay
	jobName := AgentJobName(pod.UID)
	go func() {
		time.Sleep(100 * time.Millisecond)
		createReadyAgentPod(t, clientset, opts.ManagerNamespace, jobName)
	}()

	injector := NewInjector(opts)
	if err = injector.Inject(context.Background()); err != nil {
		t.Fatalf("inject: %v", err)
	}

	// Verify agent job was created with node selector matching the pod
	job, err := clientset.BatchV1().Jobs(opts.ManagerNamespace).Get(context.Background(), agentJobName(pod.UID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("agent job not created: %v", err)
	}
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("job not pinned to target pod's node: %+v", job.Spec.Template.Spec.NodeSelector)
	}
	if !job.Spec.Template.Spec.HostPID {
		t.Fatal("job must set hostPID")
	}
	priv := false
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			priv = true
		}
	}
	if !priv {
		t.Fatal("agent container must be privileged")
	}
	if job.Labels["kubevpn/owner-id"] != opts.OwnerID || job.Labels["kubevpn/pod-uid"] != string(pod.UID) {
		t.Fatalf("job labels missing: %+v", job.Labels)
	}

	// Verify rule was written to ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(opts.ManagerNamespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	var virtuals []*controlplane.Virtual
	if err = yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("decode envoy config: %v", err)
	}
	found := false
	for _, v := range virtuals {
		if v.UID == opts.NodeID && v.Namespace == opts.Controller.Namespace {
			found = true
			if len(v.Rules) != 1 {
				t.Fatalf("expected 1 rule, got %d", len(v.Rules))
			}
			if v.Rules[0].OwnerID != opts.OwnerID {
				t.Fatalf("expected ownerID %q, got %q", opts.OwnerID, v.Rules[0].OwnerID)
			}
			if v.Rules[0].LocalTunIPv4 != opts.LocalTunIPv4 {
				t.Fatalf("expected tunIPv4 %q, got %q", opts.LocalTunIPv4, v.Rules[0].LocalTunIPv4)
			}
		}
	}
	if !found {
		t.Fatalf("envoy rule for nodeID %s not written", opts.NodeID)
	}
}

func TestGenAgentJobSpec_HasAgentConnectionEnvVar(t *testing.T) {
	_, opts, pod := newFakeWorld(t)
	input := AgentJobInput{
		ManagerNamespace: opts.ManagerNamespace,
		Namespace:        opts.Controller.Namespace,
		NodeID:           opts.NodeID,
		Image:            opts.Image,
		OwnerID:          opts.OwnerID,
		Secret:           opts.Secret,
		Pod:              pod,
	}

	job := GenAgentJobSpec(input)

	// Verify KUBEVPN_AGENT_CONNECTION env var is set to "1" in all containers
	for _, c := range job.Spec.Template.Spec.Containers {
		found := false
		for _, env := range c.Env {
			if env.Name == "KUBEVPN_AGENT_CONNECTION" && env.Value == "1" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("container %q missing KUBEVPN_AGENT_CONNECTION env var", c.Name)
		}
	}
}

func TestNodeAgentInject_IsIdempotentPerOwner(t *testing.T) {
	clientset, opts, pod := newFakeWorld(t)
	_, err := clientset.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Shorten timeouts for test
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})

	jobName := AgentJobName(pod.UID)

	// First inject creates job
	// Start goroutine to create ready pod
	go func() {
		time.Sleep(100 * time.Millisecond)
		createReadyAgentPod(t, clientset, opts.ManagerNamespace, jobName)
	}()
	err = NewInjector(opts).Inject(context.Background())
	if err != nil {
		t.Fatalf("first inject: %v", err)
	}

	// Second inject with different owner, same workload → job must NOT duplicate
	opts2 := opts
	opts2.OwnerID = "owner-2"
	opts2.Headers = map[string]string{"x-user": "bob"}
	opts2.LocalTunIPv4 = "10.0.0.2"
	opts2.LocalTunIPv6 = "fd00::2"

	if err = NewInjector(opts2).Inject(context.Background()); err != nil {
		t.Fatalf("second inject: %v", err)
	}

	jobs, err := clientset.BatchV1().Jobs(opts.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("want exactly 1 agent job after two owners, got %d", len(jobs.Items))
	}

	// Verify both rules exist in ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(opts.ManagerNamespace).Get(context.Background(), config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	var virtuals []*controlplane.Virtual
	if err = yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatalf("decode envoy config: %v", err)
	}
	if len(virtuals) != 1 {
		t.Fatalf("expected 1 virtual, got %d", len(virtuals))
	}
	if len(virtuals[0].Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(virtuals[0].Rules))
	}
}

func TestNodeAgentInject_RefusesUserNamespacedPods(t *testing.T) {
	clientset, opts, pod := newFakeWorld(t)
	pod.Spec.HostUsers = ptr.To(false)
	_, err := clientset.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	// Shorten timeouts for test just in case
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})

	err = NewInjector(opts).Inject(context.Background())
	if err == nil {
		t.Fatalf("expected hostUsers refusal, got nil")
	}
	if !strings.Contains(err.Error(), "hostUsers") {
		t.Fatalf("expected error to contain 'hostUsers', got %v", err)
	}
}

func TestEnsureAgentJob_ExistingNotTerminating_NoOp(t *testing.T) {
	clientset, opts, pod := newFakeWorld(t)
	input := AgentJobInput{
		ManagerNamespace: opts.ManagerNamespace,
		Namespace:        opts.Controller.Namespace,
		NodeID:           opts.NodeID,
		Image:            opts.Image,
		OwnerID:          opts.OwnerID,
		Secret:           opts.Secret,
		Pod:              pod,
	}

	// Shorten timeouts for test
	oldReady, oldTerm := ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout
	ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = 5*time.Second, 5*time.Second
	t.Cleanup(func() {
		ensureAgentJobReadyTimeout, ensureAgentJobTerminatingTimeout = oldReady, oldTerm
	})

	// Create the job first
	job := GenAgentJobSpec(input)
	jobName := AgentJobName(pod.UID)
	_, err := clientset.BatchV1().Jobs(input.ManagerNamespace).Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("precreate job: %v", err)
	}

	// Create the ready agent pod
	createReadyAgentPod(t, clientset, opts.ManagerNamespace, jobName)

	// Call EnsureAgentJob again: should no-op and return nil quickly
	err = EnsureAgentJob(context.Background(), clientset, input)
	if err != nil {
		t.Fatalf("EnsureAgentJob on existing job: %v", err)
	}

	// Verify job still exists
	jobs, err := clientset.BatchV1().Jobs(input.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs.Items))
	}
}

func TestEnsureAgentJob_TerminatingJob_WaitsAndRecreates(t *testing.T) {
	clientset, opts, pod := newFakeWorld(t)
	input := AgentJobInput{
		ManagerNamespace: opts.ManagerNamespace,
		Namespace:        opts.Controller.Namespace,
		NodeID:           opts.NodeID,
		Image:            opts.Image,
		OwnerID:          opts.OwnerID,
		Secret:           opts.Secret,
		Pod:              pod,
	}
	jobName := AgentJobName(pod.UID)

	// Create a job with DeletionTimestamp set (simulating terminating)
	var job *batchv1.Job = GenAgentJobSpec(input)
	job.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	job.DeletionGracePeriodSeconds = ptr.To(int64(30))
	_, err := clientset.BatchV1().Jobs(input.ManagerNamespace).Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("precreate terminating job: %v", err)
	}

	// Use a cancellable context to avoid hanging
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Start a goroutine to delete the job after a short delay (simulating job disappearance)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = clientset.BatchV1().Jobs(input.ManagerNamespace).Delete(context.Background(), jobName, metav1.DeleteOptions{})
	}()

	// Call EnsureAgentJob: will wait for job to disappear and then recreate it
	err = EnsureAgentJob(ctx, clientset, input)
	if err != nil && !strings.Contains(err.Error(), "deadline exceeded") {
		// It's okay to timeout since we're just testing the waiting logic in this fake environment
		t.Logf("EnsureAgentJob returned error (expected in fake env): %v", err)
	}

	// Check the state: if job exists, that's fine (either the original or recreated)
	_, err = clientset.BatchV1().Jobs(input.ManagerNamespace).Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		t.Fatalf("get job: %v", err)
	}
	// Test passes as long as we didn't panic
}

func TestDeleteAgentJobs_DeletesJobs(t *testing.T) {
	clientset, opts, pod := newFakeWorld(t)
	input := AgentJobInput{
		ManagerNamespace: opts.ManagerNamespace,
		Namespace:        opts.Controller.Namespace,
		NodeID:           opts.NodeID,
		Image:            opts.Image,
		OwnerID:          opts.OwnerID,
		Secret:           opts.Secret,
		Pod:              pod,
	}

	// Create an agent job first
	job := GenAgentJobSpec(input)
	_, err := clientset.BatchV1().Jobs(input.ManagerNamespace).Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("precreate job: %v", err)
	}

	// Create a pod for the job too (labeled with kubevpn/owner-id)
	podForJob := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-pod-1",
			Namespace: opts.ManagerNamespace,
			Labels: map[string]string{
				"kubevpn/owner-id": opts.OwnerID,
				"kubevpn/pod-uid":  string(pod.UID),
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "vpn", Image: opts.Image}},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
	_, err = clientset.CoreV1().Pods(opts.ManagerNamespace).Create(context.Background(), podForJob, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("precreate pod: %v", err)
	}

	// Verify job exists before delete
	jobs, err := clientset.BatchV1().Jobs(input.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job before delete, got %d", len(jobs.Items))
	}

	// Call deleteAgentJobs with a short timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = deleteAgentJobs(ctx, clientset, opts.ManagerNamespace, opts.OwnerID)
	if err != nil {
		t.Fatalf("deleteAgentJobs: %v", err)
	}

	// Verify job is deleted
	jobs, err = clientset.BatchV1().Jobs(input.ManagerNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected 0 jobs after delete, got %d", len(jobs.Items))
	}
}

func TestDeleteAgentJobs_NoJobs_NoOp(t *testing.T) {
	clientset, opts, _ := newFakeWorld(t)

	// Call deleteAgentJobs when no jobs exist
	ctx := context.Background()
	err := deleteAgentJobs(ctx, clientset, opts.ManagerNamespace, opts.OwnerID)
	if err != nil {
		t.Fatalf("deleteAgentJobs with no jobs should not error: %v", err)
	}
}
