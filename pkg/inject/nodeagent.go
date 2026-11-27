package inject

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// nodeAgentInjector attaches to a workload without patching it: writes the
// ENVOY_CONFIG rules, then waits for the node-boundary direct listeners.
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
			return fmt.Errorf("cluster disallows privileged pods (PSA enforce=%q): enable privileged node-agent pods", enforceLevel)
		}
	}

	u := o.Controller.Object.(*unstructured.Unstructured)

	templateSpec, _, err := clusterworkload.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}
	if err = validateNodeAgentTemplate(templateSpec); err != nil {
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

	return waitNodeAgentReady(ctx, o)
}

func validateNodeAgentTemplate(templateSpec *corev1.PodTemplateSpec) error {
	if templateSpec.Spec.HostNetwork {
		return errors.New("hostNetwork workloads are not supported by node-agent")
	}
	for _, container := range templateSpec.Spec.Containers {
		if container.Name == config.ContainerSidecarVPN || container.Name == config.ContainerSidecarEnvoy {
			return errors.New("legacy sidecar-injected workloads are not supported; use a clean workload")
		}
	}
	return nil
}
