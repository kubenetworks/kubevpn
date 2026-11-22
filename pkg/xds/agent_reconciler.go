package xds

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	batchv1 "k8s.io/api/batch/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// agentJobDeleteOptions forces Background propagation: on modern Kubernetes
// the default orphaned-pod behavior would leave zombie agent pods running
// (DNAT + tunnel still active, later cleanup tears the shared rules down).
var agentJobDeleteOptions = metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}

// agentPodStartupGrace: a freshly created Job may take a while to schedule its
// pod; a missing/pending pod within this window is healthy, past it the Job is
// recreated.
const agentPodStartupGrace = 2 * time.Minute

// agentLeaseFreshness bounds how stale an agent's TUN lease heartbeat may be
// before its Job is judged dead and recreated. The agent renews its lease
// every 5s (keepAgentLeaseAlive), so 30s tolerates ~6 missed heartbeats while
// still detecting a dead tunnel within one reconcile pass or two.
const agentLeaseFreshness = 30 * time.Second

// agentPodHealthy reports whether the agent Job's pod is on track: pending
// (starting) or running counts; missing, terminating, succeeded or failed
// does not.
func agentPodHealthy(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	switch pod.Status.Phase {
	case corev1.PodPending, corev1.PodRunning:
		return true
	default:
		return false
	}
}

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
	// agentSessionAlive reports whether the agent tunnel for a target pod UID
	// is alive (lease heartbeat). Injected by TunConfigServer (in-process lease
	// store read, no k8s API call); nil means "assume alive" (tests / no leases).
	agentSessionAlive func(podUID string) bool
}

// NewAgentReconciler creates an AgentReconciler that reads Virtuals from the
// traffic-manager ConfigMap. agentSessionAlive may be nil (data-plane
// verification then always passes).
func NewAgentReconciler(
	clientset kubernetes.Interface,
	managerNamespace string,
	secret *corev1.Secret,
	agentSessionAlive func(podUID string) bool,
) *AgentReconciler {
	return &AgentReconciler{
		clientset:         clientset,
		managerNamespace:  managerNamespace,
		secret:            secret,
		interval:          5 * time.Second,
		agentSessionAlive: agentSessionAlive,
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
		interval = 5 * time.Second
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

// verifyDataPlane reports whether the target pod's agent tunnel is still alive,
// judged by its lease heartbeat: the agent renews its TUN lease every 5s, so a
// stale lease means the tunnel (and with it the DNAT data plane) is dead.
// Nil hook means "assume alive" — the reconciler must never churn a Job it
// cannot actually judge.
func (r *AgentReconciler) verifyDataPlane(podUID types.UID) bool {
	if r.agentSessionAlive == nil {
		return true
	}
	alive := r.agentSessionAlive(string(podUID))
	if !alive {
		plog.G(context.Background()).Debugf("[AgentReconciler] agent lease heartbeat stale for pod %s", podUID)
	}
	return alive
}

// findAgentPod returns the agent Job's pod, if any.
func (r *AgentReconciler) findAgentPod(ctx context.Context, jobName string) (*corev1.Pod, bool) {
	pods, err := r.clientset.CoreV1().Pods(r.managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil, false
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if agentPodHealthy(pod) {
			return pod, true
		}
	}
	return nil, false
}

func (r *AgentReconciler) reconcileOnce(ctx context.Context) error {
	virtuals, err := r.virtuals(ctx)
	if err != nil {
		return fmt.Errorf("read virtuals: %w", err)
	}

	// Collect all pods selected by any node-agent Virtual. A per-workload
	// error is logged and skipped — one bad workload must not stall
	// convergence for every other one.
	wantPods := make(map[types.UID]*corev1.Pod)
	wantOwners := make(map[types.UID]string)     // pod UID -> owner ID
	wantWorkloadNS := make(map[types.UID]string) // pod UID -> workload namespace
	wantWorkloadID := make(map[types.UID]string) // pod UID -> workload node ID (Virtual.UID)

	for _, v := range virtuals {
		if v.Selector == "" {
			continue // sidecar-mode rule, skip
		}
		if len(v.Rules) == 0 {
			continue // no rules, nothing to do
		}
		pods, err := r.clientset.CoreV1().Pods(v.Namespace).List(ctx, metav1.ListOptions{LabelSelector: v.Selector})
		if err != nil {
			plog.G(ctx).Warnf("[AgentReconciler] list pods for %s/%s failed, skipping workload: %v", v.Namespace, v.UID, err)
			continue
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			wantPods[pod.UID] = pod
			wantOwners[pod.UID] = v.Rules[0].OwnerID
			wantWorkloadNS[pod.UID] = v.Namespace
			wantWorkloadID[pod.UID] = v.UID
		}
	}

	// Delete Jobs whose pod is gone or moved nodes.
	jobs, err := r.clientset.BatchV1().Jobs(r.managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kubevpn/pod-uid", // presence: filter only agent Jobs
	})
	if err != nil {
		return fmt.Errorf("list agent jobs: %w", err)
	}

	jobExists := make(map[string]bool)
	for i := range jobs.Items {
		job := &jobs.Items[i]
		podUID := types.UID(job.Labels["kubevpn/pod-uid"])
		if podUID == "" {
			continue
		}

		pod, stillExists := wantPods[podUID]
		if !stillExists {
			plog.G(ctx).Infof("[AgentReconciler] deleting orphan job %s for pod %s", job.Name, podUID)
			if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, agentJobDeleteOptions); err != nil && !k8serrors.IsNotFound(err) {
				plog.G(ctx).Warnf("[AgentReconciler] delete orphan job %s failed: %v", job.Name, err)
			}
			continue
		}

		currentNode := job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		if currentNode != pod.Spec.NodeName {
			plog.G(ctx).Infof("[AgentReconciler] pod %s moved from %s to %s, recreating job %s",
				podUID, currentNode, pod.Spec.NodeName, job.Name)
			if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, job.Name, agentJobDeleteOptions); err != nil && !k8serrors.IsNotFound(err) {
				plog.G(ctx).Warnf("[AgentReconciler] delete job %s for reschedule failed: %v", job.Name, err)
			}
			continue
		}
		jobExists[job.Name] = true
	}

	// Converge every live pod: its Job must exist with a healthy agent pod,
	// and the agent tunnel's lease heartbeat must be fresh; otherwise delete +
	// recreate so the data plane comes back.
	for podUID, pod := range wantPods {
		jobName := inject.AgentJobName(podUID)
		var job *batchv1.Job
		if jobExists[jobName] {
			var err error
			job, err = r.clientset.BatchV1().Jobs(r.managerNamespace).Get(ctx, jobName, metav1.GetOptions{})
			if err != nil {
				job = nil
				if k8serrors.IsNotFound(err) {
					jobExists[jobName] = false // raced with deletion above
				} else {
					plog.G(ctx).Warnf("[AgentReconciler] get job %s failed, skipping: %v", jobName, err)
					continue
				}
			} else if job.DeletionTimestamp != nil {
				continue // terminating; recreate on a later pass
			} else {
				agentPod, found := r.findAgentPod(ctx, jobName)
				dead := !found
				if found {
					dead = !agentPodHealthy(agentPod)
				}
				if !found && time.Since(job.CreationTimestamp.Time) <= agentPodStartupGrace {
					dead = false // pod may still be scheduling
				}
				if dead {
					plog.G(ctx).Infof("[AgentReconciler] job %s has no healthy agent pod, recreating", jobName)
					if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, jobName, agentJobDeleteOptions); err != nil && !k8serrors.IsNotFound(err) {
						plog.G(ctx).Warnf("[AgentReconciler] delete job %s failed: %v", jobName, err)
					}
					jobExists[jobName] = false
				}
			}
		}
		if jobExists[jobName] {
			// Lease-based verification engages only after the startup grace: a
			// job created seconds ago may have a Running agent pod whose vpn
			// payload has not completed its first lease renewal yet (TLS
			// handshake + tunnel setup + first heartbeat), and churning it would
			// flap. Before the grace, pod health is the only signal.
			if time.Since(job.CreationTimestamp.Time) > agentPodStartupGrace && !r.verifyDataPlane(podUID) {
				plog.G(ctx).Infof("[AgentReconciler] data-plane verification failed for job %s, recreating", jobName)
				if err := r.clientset.BatchV1().Jobs(r.managerNamespace).Delete(ctx, jobName, agentJobDeleteOptions); err != nil && !k8serrors.IsNotFound(err) {
					plog.G(ctx).Warnf("[AgentReconciler] delete job %s after verify failure: %v", jobName, err)
				}
				jobExists[jobName] = false
			}
		}
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
		job = inject.GenAgentJobSpec(input)
		_, err := r.clientset.BatchV1().Jobs(r.managerNamespace).Create(ctx, job, metav1.CreateOptions{})
		if k8serrors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			plog.G(ctx).Warnf("[AgentReconciler] create agent job %s failed: %v", jobName, err)
			continue
		}
	}

	return nil
}
