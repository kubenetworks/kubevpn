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
	"sigs.k8s.io/yaml"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/controlplane"
)

// Reset
// 1, get all proxy-resources from configmap
// 2, cleanup all containers
func (c *ConnectOptions) Reset(ctx context.Context) error {
	err := c.LeaveProxyResources(ctx)
	if err != nil {
		log.Errorf("leave proxy resources error: %v", err)
	}

	cleanup(ctx, c.clientset, c.Namespace, config.ConfigMapPodTrafficManager, false)
	var cli *client.Client
	cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil
	}
	var networkResource types.NetworkResource
	networkResource, err = cli.NetworkInspect(ctx, config.ConfigMapPodTrafficManager, types.NetworkInspectOptions{})
	if err != nil {
		return nil
	}
	if len(networkResource.Containers) == 0 {
		return cli.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return nil
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
		log.Infof("no proxy resources found")
		return
	}
	var v = make([]*controlplane.Virtual, 0)
	str := cm.Data[config.KeyEnvoy]
	if err = yaml.Unmarshal([]byte(str), &v); err != nil {
		log.Errorf("unmarshal envoy config error: %v", err)
		return
	}
	localTunIPv4 := c.GetLocalTunIPv4()
	for _, virtual := range v {
		// deployments.apps.ry-server --> deployments.apps/ry-server
		lastIndex := strings.LastIndex(virtual.Uid, ".")
		uid := virtual.Uid[:lastIndex] + "/" + virtual.Uid[lastIndex+1:]
		log.Infof("leave resource: %s", uid)
		err = UnPatchContainer(c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, uid, localTunIPv4)
		if err != nil {
			log.Errorf("unpatch container error: %v", err)
			continue
		}
		log.Infof("leave resource: %s success", uid)
	}
	return err
}
