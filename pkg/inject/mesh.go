package inject

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	pkgresource "k8s.io/cli-runtime/pkg/resource"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
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
	err = addEnvoyConfig(o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), o.Controller.Namespace, o.NodeID, o.LocalTunIPv4, o.LocalTunIPv6, o.Headers, ports, portmap, false)
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}

	if alreadyInjected(templateSpec) {
		workload := fmt.Sprintf("%s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
		plog.G(ctx).Infof("Workload %s/%s has already been injected with sidecar", o.ManagerNamespace, workload)
		return nil
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
	return containerNames.HasAll(config.ContainerSidecarVPN, config.ContainerSidecarEnvoyProxy)
}

// UnpatchContainer removes injected sidecar containers and cleans up envoy configuration.
// Returns (empty, found) where empty indicates all rules were removed and containers cleaned up.
func UnpatchContainer(ctx context.Context, nodeID string, factory cmdutil.Factory, mapInterface v12.ConfigMapInterface, object *pkgresource.Info, isMeFunc func(isFargateMode bool, rule *controlplane.Rule) bool) (bool, error) {
	u := object.Object.(*unstructured.Unstructured)
	templateSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		plog.G(ctx).Errorf("Failed to get template spec path: %v", err)
		return false, err
	}

	workload := util.ConvertUIDToWorkload(nodeID)
	empty, found, err := removeEnvoyConfig(mapInterface, object.Namespace, nodeID, isMeFunc)
	if err != nil {
		plog.G(ctx).Errorf("Failed to remove envoy config: %v", err)
		return false, err
	}
	if !found {
		plog.G(ctx).Infof("Not found proxy resource %s", workload)
		return false, nil
	}

	plog.G(ctx).Infof("Leaving workload %s", workload)

	if !empty {
		return empty, nil
	}

	RemoveContainers(&templateSpec.Spec)
	err = patchWorkload(ctx, factory, object, templateSpec, path)
	return empty, err
}
