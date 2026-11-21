package xds

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// AgentReconciler keeps node-hosted agent Jobs aligned with the pods their
// rules select (docs/55 §6): reschedule → re-pin; pod gone → delete; manager
// restart → restore. Cross-namespace ownerRefs are forbidden, so Jobs are
// matched by label, not ownerRef.
type AgentReconciler struct {
	clientset        kubernetes.Interface
	managerNamespace string
	virtuals         func(ctx context.Context) ([]*controlplane.Virtual, error)
	secret           *corev1.Secret
	interval         time.Duration
}

// NewAgentReconciler creates an AgentReconciler that reads Virtuals from the
// traffic-manager ConfigMap.
func NewAgentReconciler(
	clientset kubernetes.Interface,
	managerNamespace string,
	secret *corev1.Secret,
) *AgentReconciler {
	return &AgentReconciler{
		clientset:        clientset,
		managerNamespace: managerNamespace,
		secret:           secret,
		interval:         30 * time.Second,
		virtuals: func(ctx context.Context) ([]*controlplane.Virtual, error) {
			cm, err := clientset.CoreV1().ConfigMaps(managerNamespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			virtuals := make([]*controlplane.Virtual, 0)
			if data, ok := cm.Data[config.KeyEnvoy]; ok && data != "" {
				if err := yaml.Unmarshal([]byte(data), &virtuals); err != nil {
					return nil, err
				}
			}
			return virtuals, nil
		},
	}
}

// Start runs the reconcile loop until ctx is done.
func (r *AgentReconciler) Start(ctx context.Context) error {
	interval := r.interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Run once immediately on startup
	if err := r.reconcileOnce(ctx); err != nil {
		plog.G(ctx).Warnf("[AgentReconciler] initial reconcile failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := r.reconcileOnce(ctx); err != nil {
				plog.G(ctx).Warnf("[AgentReconciler] reconcile failed: %v", err)
			}
		}
	}
}

func (r *AgentReconciler) reconcileOnce(ctx context.Context) error {
	virtuals, err := r.virtuals(ctx)
	if err != nil {
		return fmt.Errorf("read virtuals: %w", err)
	}

	// Collect all pods selected by any node-agent Virtual
	wantPods := make(map[types.UID]*corev1.Pod)
	wantOwners := make(map[types.UID]string)       // pod UID -> owner ID
	wantWorkloadNS := make(map[types.UID]string)   // pod UID -> workload namespace
	wantWorkloadID := make(map[types.UID]string)   // pod UID -> workload node ID (Virtual.UID)

	for _, v := range virtuals {
		if v.Selector == "" {
			continue // sidecar-mode rule, skip
		}
		if len(v.Rules) == 0 {
			continue // no rules, nothing to do
		}

		pods, err := r.clientset.CoreV1().Pods(v.Namespace).List(ctx, metav1.ListOptions{LabelSelector: v.Selector})
		if err != nil {
			return fmt.Errorf("list pods for %s/%s: %w", v.Namespace, v.UID, err)
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			wantPods[pod.UID] = pod
			// Use the first rule's owner ID for labeling; it doesn't matter
			// which one we pick since the Job is shared across all owners.
			wantOwners[pod.UID] = v.Rules[0].OwnerID
			wantWorkloadNS[pod.UID] = v.Namespace
			wantWorkloadID[pod.UID] = v.UID
		}
	}

	// List all agent Jobs in the manager namespace
	jobs, err := r.clientset.BatchV1().Jobs(r.managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kubevpn/pod-uid", // presence: filter only agent Jobs
	})
	if err != nil {
		return fmt.Errorf("list agent jobs: %w", err)
	}

	// Track which Jobs should exist
	jobExists := make(map[string]bool)

	// Delete Jobs whose pod is gone or moved nodes
	for i := range jobs.Items {
		job := &jobs.Items[i]
		podUID := types.UID(job.Labels["kubevpn/pod-uid"])
		if podUID == "" {
			continue
		}

		pod, stillExists := wantPods[podUID]
		if !stillExists {
			plog.G(ctx).Debugf("[AgentReconciler] deleting orphan job %s for pod %s", job.Name, podUID)
			err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("delete job %s: %w", job.Name, err)
			}
			continue
		}

		// Check if Job is pinned to the correct node
		currentNode := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		if currentNode != pod.Spec.NodeName {
			plog.G(ctx).Infof("[AgentReconciler] pod %s moved from %s to %s, recreating job %s",
				podUID, currentNode, pod.Spec.NodeName, job.Name)
			err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("delete job %s for reschedule: %w", job.Name, err)
			}
			continue
		}

		jobExists[job.Name] = true
	}

	// Ensure a Job for every live pod
	for podUID, pod := range wantPods {
		jobName := inject.AgentJobName(podUID)
		if jobExists[jobName] {
			continue
		}

		plog.G(ctx).Infof("[AgentReconciler] creating missing agent job %s for pod %s/%s",
			jobName, pod.Namespace, pod.Name)

		input := inject.AgentJobInput{
			ManagerNamespace: r.managerNamespace,
			Namespace:        wantWorkloadNS[podUID],
			NodeID:           wantWorkloadID[podUID],
			Image:            config.Image,
			OwnerID:          wantOwners[podUID],
			Secret:           r.secret,
			Pod:              pod,
		}

		job := inject.GenAgentJobSpec(input)
		_, err := r.clientset.BatchV1().Jobs(r.managerNamespace).Create(ctx, job, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create agent job %s/%s: %w", r.managerNamespace, jobName, err)
		}
	}

	return nil
}
