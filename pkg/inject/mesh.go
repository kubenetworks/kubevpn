package inject

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

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

	templateSpec, path, err := util.GetPodTemplateSpecPath(u)
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
		workload := fmt.Sprintf("%s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
		plog.G(ctx).Infof("Workload %s/%s has already been injected with sidecar", o.ManagerNamespace, workload)
		return nil
	}
	if alreadyInjected(templateSpec) {
		// Sidecars exist but target a different traffic-manager namespace. Their
		// envoy xds_cluster still points at "kubevpn-traffic-manager.<old-ns>",
		// which stops resolving once that manager is gone, so the sidecar loses
		// its xDS stream forever. Re-inject against the current manager namespace;
		// AddVPNAndEnvoyContainer removes the stale containers first.
		plog.G(ctx).Infof("Re-injecting sidecar into %s/%s: traffic-manager namespace changed", o.Controller.Namespace, o.Controller.Name)
	}

	enableIPv6, _ := util.DetectPodSupportIPv6(ctx, o.Factory, o.ManagerNamespace)
	AddVPNAndEnvoyContainer(templateSpec, o.Controller.Namespace, o.NodeID, enableIPv6, o.ManagerNamespace, o.Secret, o.Image)

	return patchWorkload(ctx, o.Factory, o.Controller, templateSpec, path)
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
	want := "address: " + trafficManagerAddr(managerNamespace) + "\n"
	for _, c := range templateSpec.Spec.Containers {
		if c.Name != config.ContainerSidecarEnvoy {
			continue
		}
		for _, arg := range c.Args {
			if strings.Contains(arg, want) {
				return true
			}
		}
	}
	return false
}

// UnpatchContainer removes injected sidecar containers and cleans up envoy configuration.
// Returns (empty, found) where empty indicates all rules were removed and containers cleaned up.
func UnpatchContainer(ctx context.Context, nodeID string, factory cmdutil.Factory, mapInterface v12.ConfigMapInterface, object *pkgresource.Info, ownerID string) (bool, error) {
	u := object.Object.(*unstructured.Unstructured)
	templateSpec, path, err := util.GetPodTemplateSpecPath(u)
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

	RemoveContainers(&templateSpec.Spec)
	err = patchWorkload(ctx, factory, object, templateSpec, path)
	return empty, err
}
