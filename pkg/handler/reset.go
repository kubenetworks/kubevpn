package handler

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/controlplane"
	"github.com/wencaiwulue/kubevpn/v2/pkg/inject"
)

// Reset
// 1) quit daemon
// 2) get all proxy-resources from configmap
// 3) cleanup all containers
// 4) cleanup hosts
func (c *ConnectOptions) Reset(ctx context.Context) error {
	err := c.LeaveProxyResources(ctx)
	if err != nil {
		log.Errorf("Leave proxy resources error: %v", err)
	} else {
		log.Debugf("Leave proxy resources successfully")
	}

	log.Infof("Cleaning up resources")
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

	_ = c.CleanupLocalContainer(ctx)
	log.Info("Done")
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

func (c *ConnectOptions) LeaveProxyResources(ctx context.Context) (err error) {
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
		log.Infof("No proxy resources found")
		return nil
	}
	var v = make([]*controlplane.Virtual, 0)
	str := cm.Data[config.KeyEnvoy]
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		log.Errorf("Unmarshal envoy config error: %v", err)
		return
	}
	v4, _ := c.GetLocalTunIP()
	for _, virtual := range v {
		var found bool
		for _, rule := range virtual.Rules {
			if rule.LocalTunIPv4 == v4 {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		// deployments.apps.ry-server --> deployments.apps/ry-server
		lastIndex := strings.LastIndex(virtual.Uid, ".")
		uid := virtual.Uid[:lastIndex] + "/" + virtual.Uid[lastIndex+1:]
		err = inject.UnPatchContainer(c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, uid, v4)
		if err != nil {
			log.Errorf("Failed to leave workload %s: %v", uid, err)
			continue
		}
	}
	return err
}
