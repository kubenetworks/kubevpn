package handler

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// Uninstall
// 1) quit daemon
// 2) get all proxy-resources from configmap
// 3) cleanup all containers
// 4) cleanup hosts
func (c *ConnectOptions) Uninstall(ctx context.Context) error {
	plog.G(ctx).Infof("Cleaning up resources")
	ns := c.Namespace
	name := config.ConfigMapPodTrafficManager
	options := metav1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
	_ = c.clientset.CoreV1().ConfigMaps(ns).Delete(ctx, name, options)
	_ = c.clientset.CoreV1().Pods(ns).Delete(ctx, config.CniNetName, options)
	_ = c.clientset.CoreV1().Secrets(ns).Delete(ctx, name, options)
	_ = c.clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, name+"."+ns, options)
	_ = c.clientset.RbacV1().RoleBindings(ns).Delete(ctx, name, options)
	_ = c.clientset.CoreV1().ServiceAccounts(ns).Delete(ctx, name, options)
	_ = c.clientset.RbacV1().Roles(ns).Delete(ctx, name, options)
	_ = c.clientset.CoreV1().Services(ns).Delete(ctx, name, options)
	_ = c.clientset.AppsV1().Deployments(ns).Delete(ctx, name, options)
	_ = c.clientset.BatchV1().Jobs(ns).Delete(ctx, name, options)
	_ = c.CleanupLocalContainer(ctx)
	plog.G(ctx).Info("Done")
	return nil
}

func (c *ConnectOptions) CleanupLocalContainer(ctx context.Context) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	var networkResource types.NetworkResource
	networkResource, err = cli.NetworkInspect(ctx, config.ConfigMapPodTrafficManager, types.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	if len(networkResource.Containers) == 0 {
		err = cli.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return err
}

func (c *ConnectOptions) LeaveAllProxyResources(ctx context.Context) (err error) {
	if c == nil || c.clientset == nil {
		return
	}

	mapInterface := c.clientset.CoreV1().ConfigMaps(c.Namespace)
	var cm *corev1.ConfigMap
	cm, err = mapInterface.Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return
	}
	if cm == nil || cm.Data == nil || len(cm.Data[config.KeyEnvoy]) == 0 {
		plog.G(ctx).Infof("No proxy resources found")
		return nil
	}
	var v = make([]*controlplane.Virtual, 0)
	str := cm.Data[config.KeyEnvoy]
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		plog.G(ctx).Errorf("Unmarshal envoy config error: %v", err)
		return
	}
	v4, _ := c.GetLocalTunIP()
	return c.LeaveResource(ctx, c.ProxyResources().ToResources(), v4)
}

func (c *ConnectOptions) LeaveResource(ctx context.Context, resources []Resources, v4 string) error {
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
		empty, err = inject.UnPatchContainer(ctx, nodeID, c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), controller, func(isFargateMode bool, rule *controlplane.Rule) bool {
			if isFargateMode {
				return c.IsMe(workload.Namespace, util.ConvertWorkloadToUid(workload.Workload), rule.Headers)
			}
			return rule.LocalTunIPv4 == v4
		})
		if err != nil {
			plog.G(ctx).Errorf("Failed to leave workload %s in namespace %s: %v", workload.Workload, workload.Namespace, err)
			errs = append(errs, err)
			continue
		}
		if empty && util.IsK8sService(object) {
			err = inject.ModifyServiceTargetPort(ctx, c.clientset, workload.Namespace, object.Name, map[int32]int32{})
			errs = append(errs, err)
		}
		c.LeavePortMap(workload.Namespace, workload.Workload)
	}
	return errors.NewAggregate(errs)
}
