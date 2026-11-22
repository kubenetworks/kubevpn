package inject

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/utils/ptr"

	clusterworkload "github.com/wencaiwulue/kubevpn/v2/pkg/cluster/workload"
	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type meshInjector struct {
	opts InjectOptions
}

func (m *meshInjector) Inject(ctx context.Context) error {
	o := m.opts
	plog.G(ctx).Debugf("Injecting mesh (VPN+Envoy) sidecar into %s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
	u := o.Controller.Object.(*unstructured.Unstructured)

	templateSpec, path, err := clusterworkload.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	ports, portmap := collectPorts(templateSpec, o.PortMaps)
	err = addEnvoyConfig(ctx, o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), envoyRuleSpec{
		Namespace:    o.Controller.Namespace,
		NodeID:       o.NodeID,
		LocalTunIPv4: o.LocalTunIPv4,
		LocalTunIPv6: o.LocalTunIPv6,
		Headers:      o.Headers,
		Ports:        ports,
		PortMap:      portmap,
		OwnerID:      o.OwnerID,
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}

	if injectedForManager(templateSpec, o.ManagerNamespace) {
		// Dual verification: template says injected, but check actual pods too.
		// If template is stale (leave just removed sidecars but controller object is cached),
		// actual pods won't have sidecars and we need to continue injecting.
		if actualPodsHaveSidecars(ctx, o.Clientset, o.Controller, o.ManagerNamespace) {
			workload := fmt.Sprintf("%s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
			plog.G(ctx).Infof("Workload %s/%s has already been injected with sidecar", o.ManagerNamespace, workload)
			return nil
		}
		plog.G(ctx).Infof("Controller template appears injected, but actual pods do not have sidecars; proceeding with injection")
	}
	if alreadyInjected(templateSpec) {
		// Sidecars exist but target a different traffic-manager namespace. Their
		// envoy xds_cluster still points at "kubevpn-traffic-manager.<old-ns>",
		// which stops resolving once that manager is gone, so the sidecar loses
		// its xDS stream forever. Re-inject against the current manager namespace;
		// AddVPNAndEnvoyContainer removes the stale containers first.
		plog.G(ctx).Infof("Re-injecting sidecar into %s/%s: traffic-manager namespace changed", o.Controller.Namespace, o.Controller.Name)
	}

	var enableIPv6 bool
		if o.Factory != nil {
			enableIPv6, _ = util.DetectPodSupportIPv6(ctx, o.Factory, o.ManagerNamespace)
		}
	AddVPNAndEnvoyContainer(templateSpec, o.Controller.Namespace, o.NodeID, enableIPv6, o.ManagerNamespace, o.Secret, o.Image)

	// inject: undo on rollout failure to restore the working (pre-injection) revision.
	return patchWorkload(ctx, o.Factory, o.Controller, templateSpec, path, true)
}

func alreadyInjected(templateSpec *v1.PodTemplateSpec) bool {
	containerNames := sets.New[string]()
	for _, container := range templateSpec.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	return containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoy)
}

// injectedForManager reports whether the workload already carries the kubevpn
// sidecars AND the envoy sidecar's xDS bootstrap targets managerNamespace.
//
// The traffic-manager address (kubevpn-traffic-manager.<ns>) is rendered into the
// envoy --config-yaml at injection time, so it is fixed to whatever namespace the
// manager lived in when the workload was injected. If the manager later moves
// namespaces (e.g. per-namespace manager → centralized manager), a plain
// alreadyInjected check would skip re-injection, leaving the envoy xds_cluster
// pointing at a now-deleted Service whose DNS no longer resolves — the sidecar
// then loses its xDS stream permanently and serves stale routes. Detecting the
// mismatch here forces a clean re-injection against the current manager.
func injectedForManager(templateSpec *v1.PodTemplateSpec, managerNamespace string) bool {
	if !alreadyInjected(templateSpec) {
		return false
	}
	// Match the rendered "address: <traffic-manager>" line independent of
	// indentation and line ending. The envoy config is embedded via go:embed,
	// so a Windows checkout (autocrlf) gives it CRLF endings — anchoring the
	// match on a trailing "\n" would then miss the "...\r\n" line. Compare each
	// trimmed line for exact equality, which also avoids prefix collisions
	// (e.g. namespace "default" vs "default-foo").
	want := "address: " + trafficManagerAddr(managerNamespace)
	for _, c := range templateSpec.Spec.Containers {
		if c.Name != config.ContainerSidecarEnvoy {
			continue
		}
		for _, arg := range c.Args {
			for _, line := range strings.Split(arg, "\n") {
				if strings.TrimSpace(line) == want {
					return true
				}
			}
		}
	}
	return false
}

// actualPodsHaveSidecars checks if the actual running pods (not just the controller template)
// have the sidecar containers injected. This is a safety check to avoid skipping injection
// when the controller object is stale (cached) but the real pods have been reverted to
// a clean state (e.g. during leave → re-inject race window).
func actualPodsHaveSidecars(ctx context.Context, clientset kubernetes.Interface, controller *pkgresource.Info, managerNamespace string) bool {
	if clientset == nil || controller == nil || controller.Object == nil {
		return true // Can't verify, trust the template
	}

	u, ok := controller.Object.(*unstructured.Unstructured)
	if !ok {
		return true // Can't verify, trust the template
	}

	// Extract pod selector from controller
	_, selector, err := polymorphichelpers.SelectorsForObject(u)
	if err != nil {
		plog.G(ctx).Debugf("Failed to get selector for controller: %v", err)
		return true // Can't verify, trust the template
	}
	if selector.Empty() {
		return true // No selector, trust the template
	}

	// List pods using the selector
	podList, err := clientset.CoreV1().Pods(controller.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		plog.G(ctx).Debugf("Failed to list pods for controller: %v", err)
		return true // Can't verify, trust the template
	}
	if len(podList.Items) == 0 {
		plog.G(ctx).Debugf("No running pods found for controller; will verify by template only")
		return true // No pods, trust the template
	}

	// Check at least one pod - if any pod doesn't have sidecars, we need to re-inject
	for _, pod := range podList.Items {
		// Skip terminating pods
		if pod.DeletionTimestamp != nil {
			continue
		}
		// Check if this pod has sidecars
		if !podHasSidecars(&pod) {
			plog.G(ctx).Infof("Controller template appears injected, but pod %s/%s has no sidecars; will re-inject", pod.Namespace, pod.Name)
			return false
		}
		// Also verify it's injected for the current manager namespace
		if !podInjectedForManager(&pod, managerNamespace) {
			plog.G(ctx).Infof("Controller template appears injected, but pod %s/%s is not injected for current manager namespace; will re-inject", pod.Namespace, pod.Name)
			return false
		}
	}

	plog.G(ctx).Debugf("Verified %d running pods have sidecars injected", len(podList.Items))
	return true
}

// podHasSidecars checks if a single Pod spec has both sidecar containers.
func podHasSidecars(pod *v1.Pod) bool {
	containerNames := sets.New[string]()
	for _, container := range pod.Spec.Containers {
		containerNames.Insert(container.Name)
	}
	return containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoy)
}

// podInjectedForManager checks if a Pod's envoy container is configured for the given manager namespace.
func podInjectedForManager(pod *v1.Pod, managerNamespace string) bool {
	if !podHasSidecars(pod) {
		return false
	}
	want := "address: " + trafficManagerAddr(managerNamespace)
	for _, c := range pod.Spec.Containers {
		if c.Name != config.ContainerSidecarEnvoy {
			continue
		}
		for _, arg := range c.Args {
			for _, line := range strings.Split(arg, "\n") {
				if strings.TrimSpace(line) == want {
					return true
				}
			}
		}
	}
	return false
}

// UnpatchContainer removes injected sidecar containers and cleans up envoy configuration.
// Returns (empty, found) where empty indicates all rules were removed and containers cleaned up.
func UnpatchContainer(ctx context.Context, nodeID string, factory cmdutil.Factory, mapInterface v12.ConfigMapInterface, object *pkgresource.Info, ownerID string, clientset kubernetes.Interface, managerNamespace string) (bool, error) {
	u := object.Object.(*unstructured.Unstructured)
	templateSpec, path, err := clusterworkload.GetPodTemplateSpecPath(u)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get template spec path: %v", err)
		return false, err
	}

	workload := util.ConvertUIDToWorkload(nodeID)
	empty, found, err := removeEnvoyConfig(ctx, mapInterface, object.Namespace, nodeID, ownerID)
	if err != nil {
		plog.G(ctx).Errorf("Failed to remove envoy config: %v", err)
		return false, err
	}
	if !found {
		plog.G(ctx).Infof("Not found proxy resource %s", workload)
		return false, nil
	}

	plog.G(ctx).Debugf("Removing proxy from workload %q", workload)

	if !empty {
		return empty, nil
	}

	// Only patch the workload if it actually has kubevpn containers
	if alreadyInjected(templateSpec) {
		RemoveContainers(&templateSpec.Spec)
		// unpatch/leave: do NOT undo on rollout failure — the previous revision still
		// has the sidecar, so undo would re-apply what we just removed.
		err = patchWorkload(ctx, factory, object, templateSpec, path, false)
		if err != nil {
			return empty, err
		}
	}

	// node-hosted agent Jobs are deleted when this owner's last rule is removed
	// (docs/55 §6). For sidecar-mode workloads there are no such Jobs and this is
	// a no-op; the workload patch above is likewise a no-op in node-agent mode.
	if err := deleteAgentJobs(ctx, clientset, managerNamespace, ownerID); err != nil {
		plog.G(ctx).Warnf("delete agent jobs for owner %s: %v", ownerID, err)
	}

	return empty, nil
}

// deleteAgentJobs removes every agent Job owned by ownerID. Best-effort by
// callers; the manager lease reaper is the backstop (docs/55 §6).
func deleteAgentJobs(ctx context.Context, cs kubernetes.Interface, managerNamespace, ownerID string) error {
	jobs, err := cs.BatchV1().Jobs(managerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"kubevpn/owner-id": ownerID}}),
	})
	if err != nil {
		return fmt.Errorf("list agent jobs: %w", err)
	}
	if len(jobs.Items) == 0 {
		return nil
	}

	// Delete the jobs first
	deleteOptions := metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationBackground)}
	for i := range jobs.Items {
		if err := cs.BatchV1().Jobs(managerNamespace).Delete(ctx, jobs.Items[i].Name, deleteOptions); err != nil && !errors.IsNotFound(err) {
			plog.G(ctx).Warnf("delete agent job %s: %v", jobs.Items[i].Name, err)
			// Continue trying to delete other jobs even if one fails
		}
	}

	// Best-effort wait for pods to terminate (up to 10 seconds)
	// This helps ensure old agent pods release their DHCP leases before returning
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	podSelector := metav1.FormatLabelSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"kubevpn/owner-id": ownerID},
	})

	for waitCtx.Err() == nil {
		pods, err := cs.CoreV1().Pods(managerNamespace).List(waitCtx, metav1.ListOptions{
			LabelSelector: podSelector,
		})
		if err != nil {
			// If we can't list pods, just return early - this is best-effort
			plog.G(ctx).Debugf("list agent pods for cleanup: %v", err)
			return nil
		}
		if len(pods.Items) == 0 {
			// All pods are gone
			return nil
		}
		// Check if all remaining pods are terminating
		allTerminating := true
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp == nil {
				allTerminating = false
				break
			}
		}
		if allTerminating {
			plog.G(ctx).Debugf("all %d agent pods are terminating, returning early", len(pods.Items))
			return nil
		}
		// Wait a bit before checking again
		select {
		case <-waitCtx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	plog.G(ctx).Debugf("timeout waiting for agent pods to terminate, returning anyway")
	return nil
}
