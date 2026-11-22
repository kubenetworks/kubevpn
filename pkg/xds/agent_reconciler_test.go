package xds

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
)

const virtualsYAML = `- schemaVersion: 2
  Namespace: default
  Uid: deployments.apps.reviews
  Selector: app=reviews
  Rules:
  - ownerID: owner-1
    LocalTunIPv4: 198.18.0.1
    LocalTunIPv6: 2001:2::1
    PortMap:
      "9080": "9080"
    isAgent: true
`

func newReconcilerTestEnv(t *testing.T, objects ...runtime.Object) (*AgentReconciler, *fake.Clientset) {
	t.Helper()
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kubevpn"}}
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: config.ConfigMapPodTrafficManager, Namespace: "kubevpn"},
		Data:       map[string]string{config.KeyEnvoy: virtualsYAML},
	}
	all := append([]runtime.Object{ns, cm}, objects...)
	clientset := fake.NewSimpleClientset(all...)
	r := NewAgentReconciler(clientset, "kubevpn", nil, nil)
	return r, clientset
}

func targetPod(uid, name string, phase v1.PodPhase, created time.Time) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               types.UID("uid-" + uid),
			Labels:            map[string]string{"app": "reviews"},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

func agentJobFor(pod *v1.Pod) *batchv1.Job {
	return inject.GenAgentJobSpec(inject.AgentJobInput{
		ManagerNamespace: "kubevpn",
		Namespace:        pod.Namespace,
		NodeID:           "deployments.apps.reviews",
		Image:            config.Image,
		OwnerID:          "owner-1",
		Pod:              pod,
	})
}

func agentPodFor(job *batchv1.Job, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-abc",
			Namespace: "kubevpn",
			Labels:    map[string]string{"job-name": job.Name},
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

func getJob(t *testing.T, cs *fake.Clientset, name string) *batchv1.Job {
	t.Helper()
	job, err := cs.BatchV1().Jobs("kubevpn").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job %s: %v", name, err)
	}
	return job
}

func TestReconcileOnce_DeletesOrphanAndCreatesForNewPod(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	oldPod := targetPod("old", "reviews-old", v1.PodRunning, now)
	job := agentJobFor(oldPod)
	r, cs := newReconcilerTestEnv(t, oldPod, job)

	// Target pod is replaced: old gone, new running.
	newPod := targetPod("new", "reviews-new", v1.PodRunning, now)
	_ = cs.CoreV1().Pods("default").Delete(ctx, oldPod.Name, metav1.DeleteOptions{})
	_, _ = cs.CoreV1().Pods("default").Create(ctx, newPod, metav1.CreateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if _, err := cs.BatchV1().Jobs("kubevpn").Get(ctx, inject.AgentJobName(oldPod.UID), metav1.GetOptions{}); err == nil {
		t.Fatal("orphan job for deleted pod should be removed")
	}
	getJob(t, cs, inject.AgentJobName(newPod.UID)) // must exist
}

func TestReconcileOnce_ToleratesStartingAgentPod(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	r, cs := newReconcilerTestEnv(t, pod, job)
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodPending), metav1.CreateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	getJob(t, cs, job.Name) // starting pod must NOT trigger recreate
}

func TestReconcileOnce_RecreatesJobWithDeadAgentPod(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	stale := job.DeepCopy()
	stale.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute)) // past startup grace
	r, cs := newReconcilerTestEnv(t, pod, stale)
	// no agent pod at all

	// Track job deletions
	deleted := false
	cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if !deleted {
		t.Fatal("expected job to be deleted and recreated, but no deletion occurred")
	}
	getJob(t, cs, job.Name) // recreated
}

func TestReconcileOnce_SkipsFailingWorkloadWithoutStallingOthers(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	r, cs := newReconcilerTestEnv(t, pod)
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "boom" {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})
	// Second Virtual pointing at the broken namespace.
	cm, _ := cs.CoreV1().ConfigMaps("kubevpn").Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	cm.Data[config.KeyEnvoy] = virtualsYAML + `- schemaVersion: 2
  Namespace: boom
  Uid: deployments.apps.broken
  Selector: app=broken
  Rules:
  - ownerID: owner-2
    LocalTunIPv4: 198.18.0.9
    PortMap:
      "9080": "9080"
    isAgent: true
`
	_, _ = cs.CoreV1().ConfigMaps("kubevpn").Update(ctx, cm, metav1.UpdateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce must not abort on per-workload failure: %v", err)
	}
	getJob(t, cs, inject.AgentJobName(pod.UID)) // healthy workload still converged
}

func TestReconcileOnce_ToleratesZombiePodWhenHealthyExists(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	r, cs := newReconcilerTestEnv(t, pod, job)
	zombie := agentPodFor(job, v1.PodRunning)
	term := metav1.Now()
	zombie.DeletionTimestamp = &term // orphaned pod from a previous job generation
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, zombie, metav1.CreateOptions{})
	healthy := agentPodFor(job, v1.PodRunning)
	healthy.Name = job.Name + "-healthy"
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, healthy, metav1.CreateOptions{})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	getJob(t, cs, job.Name) // healthy pod found among zombies → no recreate
}

func TestReconcileOnce_KeepsJobWhileAgentLeaseAlive(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	stale := job.DeepCopy()
	stale.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute)) // past startup grace: lease check engages
	r, cs := newReconcilerTestEnv(t, pod, stale)
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.agentSessionAlive = func(podUID string) bool { return podUID == string(pod.UID) }

	deleted := false
	cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if deleted {
		t.Fatal("fresh agent lease heartbeat must keep the job")
	}
	getJob(t, cs, job.Name)
}

func TestReconcileOnce_TrustsFreshJobDespiteStaleLease(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	fresh := job.DeepCopy()
	fresh.CreationTimestamp = metav1.NewTime(now) // created "now": inside the startup grace
	r, cs := newReconcilerTestEnv(t, pod, fresh)
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.agentSessionAlive = func(podUID string) bool { return false } // heartbeat not landed yet

	deleted := false
	cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if deleted {
		t.Fatal("a job inside the startup grace must be trusted despite a stale lease")
	}
	getJob(t, cs, job.Name)
}

func TestReconcileOnce_RecreatesJobWhenLeaseStale(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	stale := job.DeepCopy()
	stale.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute)) // past startup grace: lease check engages
	r, cs := newReconcilerTestEnv(t, pod, stale)
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.agentSessionAlive = func(podUID string) bool { return false } // heartbeat stale

	deleted := false
	cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if !deleted {
		t.Fatal("stale agent lease heartbeat must delete and recreate the job")
	}
	getJob(t, cs, job.Name) // recreated
}

func TestReconcileOnce_DeletesJobsWithBackgroundPropagation(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	oldPod := targetPod("old", "reviews-old", v1.PodRunning, now)
	job := agentJobFor(oldPod)
	r, cs := newReconcilerTestEnv(t, oldPod, job)
	newPod := targetPod("new", "reviews-new", v1.PodRunning, now)
	_ = cs.CoreV1().Pods("default").Delete(ctx, oldPod.Name, metav1.DeleteOptions{})
	_, _ = cs.CoreV1().Pods("default").Create(ctx, newPod, metav1.CreateOptions{})

	var policy *metav1.DeletionPropagation
	cs.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		da := action.(k8stesting.DeleteAction)
		opts := da.GetDeleteOptions()
		policy = opts.PropagationPolicy
		return false, nil, nil // let the default reactor handle deletion
	})
	_ = r.reconcileOnce(ctx)
	if policy == nil || *policy != metav1.DeletePropagationBackground {
		t.Fatalf("job delete must use Background propagation, got %v", policy)
	}
}
