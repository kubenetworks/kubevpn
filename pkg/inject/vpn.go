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
	err = addEnvoyConfig(ctx, o.Clientset.CoreV1().ConfigMaps(o.ManagerNamespace), envoyRuleSpec{
		Namespace:    o.Controller.Namespace,
		NodeID:       o.NodeID,
		LocalTunIPv4: o.LocalTunIPv4,
		LocalTunIPv6: o.LocalTunIPv6,
		Ports:        ports,
		PortMap:      portmap,
		OwnerID:      o.OwnerID,
	})
	if err != nil {
		plog.G(ctx).Errorf("Failed to add envoy config: %v", err)
		return err
	}

	AddVPNContainer(&podTempSpec.Spec, o.LocalTunIPv4, o.LocalTunIPv6, o.ManagerNamespace, o.Secret, o.Image)

	return patchWorkload(ctx, o.Factory, o.Controller, podTempSpec, path)
}
