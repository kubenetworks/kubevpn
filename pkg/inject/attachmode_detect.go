package inject

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const (
	psaLabelPrefix = "pod-security.kubernetes.io/"
	psaEnforce     = psaLabelPrefix + "enforce"
	psaPrivileged  = "privileged"
	psaBaseline    = "baseline"
	psaRestricted  = "restricted"

	probeJobNamePrefix = "kubevpn-attachmode-probe-"
)

var (
	// ProbeTimeout is the total time to wait for the canary probe to succeed
	ProbeTimeout = 60 * time.Second
	// ProbePollInterval is how often to poll the canary pod status
	ProbePollInterval = 2 * time.Second
)

// AttachModeDetector caches the detection result to avoid repeated probes
type AttachModeDetector struct {
	clientset kubernetes.Interface
	namespace string

	// cached result
	cached      bool
	viable      bool
	reason      string
}

// NewAttachModeDetector creates a detector that caches its result
func NewAttachModeDetector(clientset kubernetes.Interface, namespace string) *AttachModeDetector {
	return &AttachModeDetector{
		clientset: clientset,
		namespace: namespace,
	}
}

// Detect checks if node-agent mode is viable, using cached result if available
func (d *AttachModeDetector) Detect(ctx context.Context) (viable bool, reason string) {
	if d.cached {
		return d.viable, d.reason
	}

	d.viable, d.reason = DetectNodeAgentViability(ctx, d.clientset, d.namespace)
	d.cached = true
	return d.viable, d.reason
}

// DetectNodeAgentViability checks if node-agent mode is viable.
// It first uses the fast path (PSA labels), then the slow path (canary Job) if needed.
// Returns true if node-agent is viable, false otherwise. The reason string explains why.
func DetectNodeAgentViability(ctx context.Context, clientset kubernetes.Interface, managerNamespace string) (viable bool, reason string) {
	// Fast path: Check PSA labels
	viable, reason = checkPSALabels(ctx, clientset, managerNamespace)
	if viable {
		return true, reason
	}

	// If PSA labels say restricted/baseline, trust that and don't try canary
	if reason != "" {
		return false, reason
	}

	// Slow path: Try canary Job
	return checkCanaryJob(ctx, clientset, managerNamespace)
}

// checkPSALabels checks the namespace's Pod Security Admission labels.
// Returns (true, reason) if privileged, (false, reason) if restricted/baseline.
// Returns (false, "") if no labels or unknown value (need to try canary).
func checkPSALabels(ctx context.Context, clientset kubernetes.Interface, namespace string) (bool, string) {
	ns, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Debugf("Failed to get namespace %s for PSA check: %v", namespace, err)
		return false, ""
	}

	level := ns.Labels[psaEnforce]
	if level == "" {
		plog.G(ctx).Debugf("Namespace %s has no %s label", namespace, psaEnforce)
		return false, ""
	}

	switch level {
	case psaPrivileged:
		return true, fmt.Sprintf("PSA %s=%s", psaEnforce, psaPrivileged)
	case psaBaseline:
		return false, fmt.Sprintf("PSA %s=%s (privileged containers disallowed)", psaEnforce, psaBaseline)
	case psaRestricted:
		return false, fmt.Sprintf("PSA %s=%s (privileged containers disallowed)", psaEnforce, psaRestricted)
	default:
		plog.G(ctx).Debugf("Namespace %s has unknown PSA level %s, will try canary", namespace, level)
		return false, ""
	}
}

// checkCanaryJob attempts to run a privileged Job to see if it's allowed.
func checkCanaryJob(ctx context.Context, clientset kubernetes.Interface, namespace string) (viable bool, reason string) {
	jobName := probeJobName()

	plog.G(ctx).Infof("Testing node-agent viability with canary Job %s/%s", namespace, jobName)

	// Clean up job when done (best effort)
	defer func() {
		// Use background context since original ctx might be done
		deleteCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := clientset.BatchV1().Jobs(namespace).Delete(deleteCtx, jobName, metav1.DeleteOptions{
			PropagationPolicy:  &[]metav1.DeletionPropagation{metav1.DeletePropagationBackground}[0],
			GracePeriodSeconds: &[]int64{0}[0],
		})
		if err != nil && !errors.IsNotFound(err) {
			plog.G(context.Background()).Debugf("Failed to clean up canary Job %s: %v", jobName, err)
		}
	}()

	// Create canary Job
	job := newProbeJob(jobName, namespace)
	_, err := clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if errors.IsForbidden(err) {
			return false, fmt.Sprintf("canary Job blocked: %v", err)
		}
		plog.G(ctx).Debugf("Canary Job creation failed unexpectedly: %v", err)
		return false, fmt.Sprintf("canary Job creation failed: %v", err)
	}

	// Wait for pod to start running or fail
	return waitForProbePod(ctx, clientset, namespace, jobName)
}

func probeJobName() string {
	return fmt.Sprintf("%s%08x", probeJobNamePrefix, rand.Uint32())
}

// newProbeJob creates a minimal privileged Job that runs to completion immediately.
// Uses registry.k8s.io/pause:3.9 (lightweight, almost always cached on nodes).
// The pause container exits immediately → pod becomes Succeeded quickly.
// Note: If pause image is not cached and cluster has no external network access,
// pod will stay Pending → probe times out → returns false (acceptable, as this
// indicates the cluster cannot run the privileged node-agent pod anyway).
func newProbeJob(name, namespace string) *batchv1.Job {
	privileged := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				config.ManageBy: config.ConfigMapPodTrafficManager,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](1),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						config.ManageBy: config.ConfigMapPodTrafficManager,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "probe",
							Image:   "registry.k8s.io/pause:3.9",
							Command: []string{"/pause"},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
						},
					},
				},
			},
		},
	}
}

// waitForProbePod waits for the probe pod to start. Returns true if it started successfully.
func waitForProbePod(ctx context.Context, clientset kubernetes.Interface, namespace, jobName string) (bool, string) {
	// Create a context with timeout for the probe
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	selector := fmt.Sprintf("%s=%s", config.ManageBy, config.ConfigMapPodTrafficManager)

	var lastErr error
	for {
		select {
		case <-probeCtx.Done():
			if lastErr != nil {
				return false, fmt.Sprintf("probe timed out after %v: %v", ProbeTimeout, lastErr)
			}
			return false, fmt.Sprintf("probe timed out after %v", ProbeTimeout)
		case <-time.After(ProbePollInterval):
		}

		// List pods for the job
		pods, err := clientset.CoreV1().Pods(namespace).List(probeCtx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			lastErr = err
			continue
		}

		// Find our pod by job name (since labels might match other things)
		var ourPod *corev1.Pod
		for i := range pods.Items {
			pod := &pods.Items[i]
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "Job" && owner.Name == jobName {
					ourPod = pod
					break
				}
			}
		}

		if ourPod == nil {
			// Pod not created yet, keep waiting
			continue
		}

		// Check pod phase
		switch ourPod.Status.Phase {
		case corev1.PodRunning:
			// Pod is running - success!
			return true, fmt.Sprintf("canary Job pod %s ran successfully", ourPod.Name)
		case corev1.PodSucceeded:
			// Pod completed successfully
			return true, fmt.Sprintf("canary Job pod %s succeeded", ourPod.Name)
		case corev1.PodFailed:
			// Check why it failed
			reason := "canary Job pod failed"
			if len(ourPod.Status.Conditions) > 0 {
				for _, cond := range ourPod.Status.Conditions {
					if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
						reason = fmt.Sprintf("canary Job pod %s not scheduled: %s", ourPod.Name, cond.Message)
						break
					}
				}
			}
			// Also check container statuses for Forbidden
			for _, cs := range ourPod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					reason = fmt.Sprintf("canary Job pod %s container %s: %s", ourPod.Name, cs.Name, cs.State.Waiting.Message)
					break
				}
			}
			return false, reason
		case corev1.PodPending:
			// Check if it's forbidden
			for _, cond := range ourPod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
					if cond.Reason == "Unschedulable" && (cond.Message != "" && len(cond.Message) > 0) {
						return false, fmt.Sprintf("canary Job pod unschedulable: %s", cond.Message)
					}
				}
			}
			// Still pending, keep waiting
			continue
		}
	}
}
