package xds

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
)

// seedWorld builds: ConfigMap with one Virtual carrying Selector "app=web",
// pod web-1 on node-a with matching labels, agent Job for its UID.
func seedWorld(t *testing.T) (*fake.Clientset, *AgentReconciler) {
	t.Helper()
	const managerNS = "kubevpn"
	const workloadNS = "default"
	const podUID = "pod-uid-1"

	virtuals := []*controlplane.Virtual{
		{
			SchemaVersion: controlplane.CurrentSchemaVersion,
			Namespace:     workloadNS,
			UID:           "deployments.apps/web",
			Selector:      "app=web",
			Ports: []controlplane.ContainerPort{
				{ContainerPort: 8080},
			},
			Rules: []*controlplane.Rule{
				{
					LocalTunIPv4: "198.18.0.1",
					LocalTunIPv6: "fd00::1",
					OwnerID:      "owner-1",
				},
			},
		},
	}
	virtualData, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: managerNS,
		},
		Data: map[string]string{
			config.KeyEnvoy: string(virtualData),
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-1",
			Namespace: workloadNS,
			UID:       podUID,
			Labels:    map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
		},
	}

	job := inject.GenAgentJobSpec(inject.AgentJobInput{
		ManagerNamespace: managerNS,
		Image:            config.Image,
		OwnerID:          "owner-1",
		Secret:           nil,
		Pod:              pod,
	})

	clientset := fake.NewSimpleClientset(cm, pod, job)

	r := NewAgentReconciler(clientset, managerNS, nil)
	return clientset, r
}

func TestReconcilerRecreatesJobAfterPodReschedule(t *testing.T) {
	clientset, r := seedWorld(t)
	ctx := context.Background()

	// Reschedule: pod now on node-b
	pod, err := clientset.CoreV1().Pods("default").Get(ctx, "web-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = "node-b"
	_, err = clientset.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The old job should be gone; a new one should exist on node-b
	jobs, err := clientset.BatchV1().Jobs("kubevpn").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job after reschedule, got %d", len(jobs.Items))
	}
	job := &jobs.Items[0]
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "node-b" {
		t.Fatalf("job not moved to node-b: %+v", job.Spec.Template.Spec.NodeSelector)
	}
}

func TestReconcilerDeletesOrphanJobWhenPodGone(t *testing.T) {
	clientset, r := seedWorld(t)
	ctx := context.Background()

	err := clientset.CoreV1().Pods("default").Delete(ctx, "web-1", metav1.DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	jobs, err := clientset.BatchV1().Jobs("kubevpn").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("orphan job must be deleted, got %d", len(jobs.Items))
	}
}

func TestReconcilerRestoresMissingJobAfterManagerRestart(t *testing.T) {
	clientset, r := seedWorld(t)
	ctx := context.Background()

	err := clientset.BatchV1().Jobs("kubevpn").DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	jobs, err := clientset.BatchV1().Jobs("kubevpn").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("job must be restored from rules, got %d", len(jobs.Items))
	}
}

func TestReconcilerSkipsSidecarModeVirtuals(t *testing.T) {
	clientset, r := seedWorld(t)
	ctx := context.Background()

	// Remove the selector to simulate a sidecar-mode Virtual
	cm, err := clientset.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var virtuals []*controlplane.Virtual
	if err := yaml.Unmarshal([]byte(cm.Data[config.KeyEnvoy]), &virtuals); err != nil {
		t.Fatal(err)
	}
	virtuals[0].Selector = "" // sidecar mode
	virtualData, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[config.KeyEnvoy] = string(virtualData)
	_, err = clientset.CoreV1().ConfigMaps("kubevpn").Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Delete the job
	err = clientset.BatchV1().Jobs("kubevpn").DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Reconcile should NOT recreate it (selector is empty)
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs, err := clientset.BatchV1().Jobs("kubevpn").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("sidecar-mode job should not be recreated, got %d", len(jobs.Items))
	}
}

func TestReconcilerDoesNotTouchUnrelatedJobs(t *testing.T) {
	clientset, r := seedWorld(t)
	ctx := context.Background()

	// Create an unrelated Job without kubevpn labels
	unrelatedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated-job",
			Namespace: "kubevpn",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	_, err := clientset.BatchV1().Jobs("kubevpn").Create(ctx, unrelatedJob, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Reconcile should leave it alone
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_, err = clientset.BatchV1().Jobs("kubevpn").Get(ctx, "unrelated-job", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unrelated job was deleted: %v", err)
	}
}

func TestReconcilerHandlesMultiplePods(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	ctx := context.Background()
	const managerNS = "kubevpn"
	const workloadNS = "default"

	// Create ConfigMap with a Virtual
	virtuals := []*controlplane.Virtual{
		{
			SchemaVersion: controlplane.CurrentSchemaVersion,
			Namespace:     workloadNS,
			UID:           "deployments.apps/web",
			Selector:      "app=web",
			Ports: []controlplane.ContainerPort{
				{ContainerPort: 8080},
			},
			Rules: []*controlplane.Rule{
				{
					LocalTunIPv4: "198.18.0.1",
					OwnerID:      "owner-1",
				},
			},
		},
	}
	virtualData, err := yaml.Marshal(virtuals)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigMapPodTrafficManager,
			Namespace: managerNS,
		},
		Data: map[string]string{
			config.KeyEnvoy: string(virtualData),
		},
	}
	_, err = clientset.CoreV1().ConfigMaps(managerNS).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Create multiple pods
	names := []string{"web-0", "web-1", "web-2"}
	uids := []types.UID{"pod-uid-0", "pod-uid-1", "pod-uid-2"}
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      names[i],
				Namespace: workloadNS,
				UID:       uids[i],
				Labels:    map[string]string{"app": "web"},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-a",
			},
		}
		_, err = clientset.CoreV1().Pods(workloadNS).Create(ctx, pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Reconcile should create Jobs for all 3 pods
	r := NewAgentReconciler(clientset, managerNS, nil)
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs, err := clientset.BatchV1().Jobs(managerNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs.Items))
	}
}
