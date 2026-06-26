package inject

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

type vpnInjector struct {
	opts InjectOptions
}

func (v *vpnInjector) Inject(ctx context.Context) error {
	o := v.opts
	plog.G(ctx).Debugf("Injecting VPN sidecar into %s/%s", o.Controller.Mapping.Resource.Resource, o.Controller.Name)
	u := o.Controller.Object.(*unstructured.Unstructured)

	podTempSpec, path, err := util.GetPodTemplateSpecPath(u)
	if err != nil {
		return err
	}

	ports, portmap := collectPorts(podTempSpec, nil)
	err = addEnvoyConfig(o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), o.Controller.Namespace, o.NodeID, o.LocalTunIPv4, o.LocalTunIPv6, nil, ports, portmap, false)
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}

	AddVPNContainer(&podTempSpec.Spec, o.LocalTunIPv4, o.LocalTunIPv6, o.ManagerNamespace, o.Secret, o.Image)

	return patchWorkload(ctx, o.Factory, o.Controller, podTempSpec, path)
}
