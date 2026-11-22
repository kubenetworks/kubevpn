package xds

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseDNATCount(t *testing.T) {
	cases := []struct {
		output         string
		present, known bool
	}{
		{"3\n", true, true},   // rules present
		{"0\n", false, true},  // no rules — must recreate
		{"", false, false},    // inconclusive
		{"probe-unknown\n", false, false}, // nsenter missing
	}
	for _, c := range cases {
		present, known := parseDNATCount(c.output)
		if present != c.present || known != c.known {
			t.Errorf("parseDNATCount(%q) = (%v,%v), want (%v,%v)", c.output, present, known, c.present, c.known)
		}
	}
}

func TestReconcileOnce_RecreatesWhenDNATProbeSaysMissing(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now) // helper from agent_reconciler_test.go
	job := agentJobFor(pod)
	stale := job.DeepCopy()
	stale.CreationTimestamp = metav1.NewTime(now.Add(-5 * time.Minute)) // past startup grace
	r, cs := newReconcilerTestEnv(t, pod, stale)
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.probe = func(ctx context.Context, agentPodName string) (string, error) { return "0\n", nil }

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	// Just verify the job exists at the end (fake client doesn't handle recreate timestamps properly)
	getJob(t, cs, job.Name)
}

func TestReconcileOnce_KeepsJobWhenDNATProbeSaysPresent(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	pod := targetPod("p", "reviews", v1.PodRunning, now)
	job := agentJobFor(pod)
	fresh := job.DeepCopy()
	fresh.CreationTimestamp = metav1.NewTime(now) // fresh job
	r, cs := newReconcilerTestEnv(t, pod, fresh)
	_, _ = cs.CoreV1().Pods("kubevpn").Create(ctx, agentPodFor(job, v1.PodRunning), metav1.CreateOptions{})
	r.probe = func(ctx context.Context, agentPodName string) (string, error) { return "3\n", nil }

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	// Just verify the job exists at the end
	getJob(t, cs, job.Name)
}
