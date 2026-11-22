package inject

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// nodeAgentInjector attaches to a workload without patching it: writes the
// same ENVOY_CONFIG rules as mesh mode, then ensures one privileged Job per
// target pod in the manager namespace.
type nodeAgentInjector struct {
	opts InjectOptions
}

func (n *nodeAgentInjector) Inject(ctx context.Context) error {
	o := n.opts

	// Check PSA labels on manager namespace: if enforce is restricted or baseline,
	// return a clean error telling users to set sidecar mode explicitly.
	if ns, err := o.Clientset.CoreV1().Namespaces().Get(ctx, o.ManagerNamespace, metav1.GetOptions{}); err != nil {
		// If we can't read the namespace (e.g., RBAC), log warning and continue
		// (let the real Job creation fail with authoritative error)
		plog.G(ctx).Warnf("[node-agent] failed to check PSA labels on namespace %s: %v", o.ManagerNamespace, err)
	} else {
		enforceLevel := ns.Labels["pod-security.kubernetes.io/enforce"]
		if enforceLevel == "restricted" || enforceLevel == "baseline" {
			return fmt.Errorf("cluster disallows privileged pods (PSA enforce=%q): set proxy.attachMode=sidecar (helm) to use sidecar injection", enforceLevel)
		}
	}

	u := o.Controller.Object.(*unstructured.Unstructured)

	templateSpec, _, err := clusterworkload.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}
	ports, portmap := collectPorts(templateSpec, o.PortMaps)

	// Extract template labels as selector for the reconciler
	var selector string
	podLabels, found, err := unstructured.NestedStringMap(u.Object, "spec", "template", "metadata", "labels")
	if err == nil && found && len(podLabels) > 0 {
		selector = labels.Set(podLabels).AsSelector().String()
	}

	// Rules are identical to mesh mode — same xDS contract, same OwnerID merge.
	if err = addEnvoyConfig(ctx, o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), envoyRuleSpec{
		Namespace:    o.Controller.Namespace,
		NodeID:       o.NodeID,
		LocalTunIPv4: o.LocalTunIPv4,
		LocalTunIPv6: o.LocalTunIPv6,
		Headers:      o.Headers,
		PortMap:      portmap,
		Ports:        ports,
		OwnerID:      o.OwnerID,
		Selector:     selector,
		IsAgent:      true,
	}); err != nil {
		return fmt.Errorf("add envoy config: %w", err)
	}

	pods, err := listControllerPods(ctx, o.Clientset, o.Controller)
	if err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		// One envoy per netns: a pod already carrying kubevpn sidecars (e.g.
		// a fargate/service-mode injection in the same proxy command) is
		// served by its own sidecar envoy — an agent Job here would run a
		// second envoy in the same netns and fight over the admin/inbound
		// ports.
		if podHasKubevpnSidecar(pod) {
			plog.G(ctx).Debugf("[node-agent] pod %s/%s already carries kubevpn sidecars, skipping agent attach",
				pod.Namespace, pod.Name)
			continue
		}
		if pod.Spec.HostUsers != nil && !*pod.Spec.HostUsers {
			return fmt.Errorf("pod %s/%s uses hostUsers: false — node-agent attach is unsupported", pod.Namespace, pod.Name)
		}
		if err = EnsureAgentJob(ctx, o.Clientset, AgentJobInput{
			ManagerNamespace: o.ManagerNamespace,
			Namespace:        o.Controller.Namespace,
			NodeID:           o.NodeID,
			Image:            o.Image,
			OwnerID:          o.OwnerID,
			Secret:           o.Secret,
			Pod:              pod,
		}); err != nil {
			return fmt.Errorf("ensure agent job for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

// listControllerPods finds the running pods of the workload via its template
// labels (pods carry the template labels; selectors alone can be absent on
// some controller kinds).
func listControllerPods(ctx context.Context, cs kubernetes.Interface, controller *resource.Info) (*v1.PodList, error) {
	u, ok := controller.Object.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("controller is not unstructured: %T", controller.Object)
	}
	podLabels, found, err := unstructured.NestedStringMap(u.Object, "spec", "template", "metadata", "labels")
	if err != nil || !found || len(podLabels) == 0 {
		return nil, fmt.Errorf("workload %s/%s has no template labels to select pods", controller.Namespace, controller.Name)
	}
	return cs.CoreV1().Pods(controller.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(podLabels).AsSelector().String(),
	})
}

// Tunable so tests can shorten the waits; production values keep injection
// synchronous but bounded.
var (
	ensureAgentJobReadyTimeout       = 30 * time.Second
	ensureAgentJobReadyPollInterval  = 500 * time.Millisecond
	ensureAgentJobTerminatingTimeout = 60 * time.Second
)

// waitForAgentJobPodReady blocks until the agent Job's pod is running with all
// containers ready, so injection returns only once the data plane (DNAT rules
// applied by the job script, envoy and tunnel payloads) is actually up.
func waitForAgentJobPodReady(ctx context.Context, cs kubernetes.Interface, namespace, jobName string) error {
	selector := labels.Set{"job-name": jobName}.AsSelector().String()
	ctxTimeout, cancel := context.WithTimeout(ctx, ensureAgentJobReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(ensureAgentJobReadyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctxTimeout.Done():
			return fmt.Errorf("agent job %q pod not ready within %s: %w", jobName, ensureAgentJobReadyTimeout, ctxTimeout.Err())
		case <-ticker.C:
			pods, err := cs.CoreV1().Pods(namespace).List(ctxTimeout, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				return fmt.Errorf("list agent pods for job %s: %w", jobName, err)
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.GetDeletionTimestamp() != nil || pod.Status.Phase != v1.PodRunning {
					continue
				}
				if agentPodAllReady(pod) {
					return nil
				}
			}
		}
	}
}

// agentPodAllReady reports whether every container status of the pod is ready.
func agentPodAllReady(pod *v1.Pod) bool {
	if len(pod.Spec.Containers) == 0 || len(pod.Status.ContainerStatuses) < len(pod.Spec.Containers) {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func EnsureAgentJob(ctx context.Context, cs kubernetes.Interface, input AgentJobInput) error {
	name := AgentJobName(input.Pod.UID)
	_, err := cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
	if err == nil {
		return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
	}
	if !errors.IsAlreadyExists(err) {
		return err
	}

	// Job exists: check if it's terminating
	job, err := cs.BatchV1().Jobs(input.ManagerNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Job disappeared between Create and Get: try again once
			_, err = cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
			if err != nil {
				return err
			}
			return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
		}
		return err
	}

	if job.DeletionTimestamp == nil {
		// Job exists and is not terminating: wait for its pod to be ready
		return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
	}

	// Job is terminating: wait for it to disappear
	plog.G(ctx).Debugf("Agent job %q is terminating, waiting for it to disappear", name)
	ctxWait, cancel := context.WithTimeout(ctx, ensureAgentJobTerminatingTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctxWait.Done():
			return fmt.Errorf("timed out waiting for terminating agent job %q: %w", name, ctxWait.Err())
		case <-ticker.C:
			_, err := cs.BatchV1().Jobs(input.ManagerNamespace).Get(ctx, name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				// Job is gone: recreate it
				plog.G(ctx).Debugf("Agent job %q disappeared, recreating", name)
				_, err = cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
				if err != nil {
					return err
				}
				return waitForAgentJobPodReady(ctx, cs, input.ManagerNamespace, name)
			}
			if err != nil {
				plog.G(ctx).Warnf("Failed to get agent job %q while waiting: %v", name, err)
			}
			// Job still exists: keep waiting
		}
	}
}

// ensureAgentJob creates the deterministic agent Job; AlreadyExists is a no-op
// (second owner, reconcile re-run).
// Deprecated: Use EnsureAgentJob instead.
func ensureAgentJob(ctx context.Context, cs kubernetes.Interface, o InjectOptions, pod *v1.Pod) error {
	input := AgentJobInput{
		ManagerNamespace: o.ManagerNamespace,
		Namespace:        o.Controller.Namespace,
		NodeID:           o.NodeID,
		Image:            o.Image,
		OwnerID:          o.OwnerID,
		Secret:           o.Secret,
		Pod:              pod,
	}
	return EnsureAgentJob(ctx, cs, input)
}

// podHasKubevpnSidecar reports whether the pod already carries kubevpn
// sidecar containers (mesh / fargate injection). Such pods are served by
// their own sidecar envoy — exactly one envoy per netns.
func podHasKubevpnSidecar(pod *v1.Pod) bool {
	for i := range pod.Spec.Containers {
		name := pod.Spec.Containers[i].Name
		if name == config.ContainerSidecarVPN || name == config.ContainerSidecarEnvoy {
			return true
		}
	}
	return false
}
