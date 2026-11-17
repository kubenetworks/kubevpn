package inject

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
)

// nodeAgentInjector attaches to a workload without patching it: writes the
// same ENVOY_CONFIG rules as mesh mode, then ensures one privileged Job per
// target pod in the manager namespace.
type nodeAgentInjector struct {
	opts InjectOptions
}

func (n *nodeAgentInjector) Inject(ctx context.Context) error {
	o := n.opts
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
	}); err != nil {
		return fmt.Errorf("add envoy config: %w", err)
	}

	pods, err := listControllerPods(ctx, o.Clientset, o.Controller)
	if err != nil {
		return fmt.Errorf("list controller pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.HostUsers != nil && !*pod.Spec.HostUsers {
			return fmt.Errorf("pod %s/%s uses hostUsers: false — node-agent attach is unsupported", pod.Namespace, pod.Name)
		}
		if err = ensureAgentJob(ctx, o.Clientset, o, pod); err != nil {
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

// EnsureAgentJob creates the deterministic agent Job; AlreadyExists is a no-op
// (second owner, reconcile re-run).
func EnsureAgentJob(ctx context.Context, cs kubernetes.Interface, input AgentJobInput) error {
	_, err := cs.BatchV1().Jobs(input.ManagerNamespace).Create(ctx, GenAgentJobSpec(input), metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ensureAgentJob creates the deterministic agent Job; AlreadyExists is a no-op
// (second owner, reconcile re-run).
// Deprecated: Use EnsureAgentJob instead.
func ensureAgentJob(ctx context.Context, cs kubernetes.Interface, o InjectOptions, pod *v1.Pod) error {
	input := AgentJobInput{
		ManagerNamespace: o.ManagerNamespace,
		Image:            o.Image,
		OwnerID:          o.OwnerID,
		Secret:           o.Secret,
		Pod:              pod,
	}
	return EnsureAgentJob(ctx, cs, input)
}

