package handler

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/errors"

	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// LeaveAllProxyResources removes all proxy sidecar injections for the current connection's workloads.
func (c *ConnectOptions) LeaveAllProxyResources(ctx context.Context) error {
	if c == nil || c.clientset == nil {
		return nil
	}
	resources := c.ProxyResources().ToResources()
	if len(resources) == 0 {
		plog.G(ctx).Infof("No proxy resources found")
		return nil
	}
	v4, _ := c.GetLocalTunIP()
	return c.LeaveResource(ctx, resources, v4)
}

// LeaveResource unpatches the given proxy resources and restores their original pod specs.
func (c *ConnectOptions) LeaveResource(ctx context.Context, resources []Resources, v4 string) error {
	plog.G(ctx).Infof("Leaving %d proxy resources with local IP %s", len(resources), v4)
	var errs []error
	for _, workload := range resources {
		// deployments.apps.ry-server --> deployments.apps/ry-server
		object, controller, err := util.GetTopOwnerObject(ctx, c.factory, workload.Namespace, workload.Workload)
		if err != nil {
			plog.G(ctx).Errorf("Failed to get unstructured object: %v", err)
			errs = append(errs, err)
			continue
		}
		nodeID := fmt.Sprintf("%s.%s", object.Mapping.Resource.GroupResource().String(), object.Name)
		var empty bool
		empty, err = inject.UnpatchContainer(ctx, nodeID, c.factory, c.clientset.CoreV1().ConfigMaps(c.ManagerNamespace), controller, func(isFargateMode bool, rule *controlplane.Rule) bool {
			if isFargateMode {
				return c.IsMe(workload.Namespace, util.ConvertWorkloadToUID(workload.Workload), rule.Headers)
			}
			return rule.LocalTunIPv4 == v4
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to leave workload %s in namespace %s: %v", workload.Workload, workload.Namespace, err)
			errs = append(errs, err)
			continue
		}
		if empty && util.IsK8sService(object) {
			if err = inject.ModifyServiceTargetPort(ctx, c.clientset, workload.Namespace, object.Name, map[int32]int32{}); err != nil {
				errs = append(errs, err)
			}
		}
		c.leavePortMap(workload.Namespace, workload.Workload)
		plog.G(ctx).Infof("Left workload %s in namespace %s", workload.Workload, workload.Namespace)
	}
	return errors.NewAggregate(errs)
}
