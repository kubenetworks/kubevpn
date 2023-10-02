package handler

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
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
	cm, err := c.clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	var v = make([]*controlplane.Virtual, 0)
	localTunIPv4 := c.GetLocalTunIPv4()
	if cm != nil && cm.Data != nil {
		if str, ok := cm.Data[config.KeyEnvoy]; ok && len(str) != 0 {
			if err = yaml.Unmarshal([]byte(str), &v); err != nil {
				log.Error(err)
				return err
			}
			for _, virtual := range v {
				// deployments.apps.ry-server --> deployments.apps/ry-server
				lastIndex := strings.LastIndex(virtual.Uid, ".")
				uid := virtual.Uid[:lastIndex] + "/" + virtual.Uid[lastIndex+1:]
				err = UnPatchContainer(c.factory, c.clientset.CoreV1().ConfigMaps(c.Namespace), c.Namespace, uid, localTunIPv4)
				if err != nil {
					log.Error(err)
					continue
				}
			}
		}
	}
	cleanup(ctx, c.clientset, c.Namespace, config.ConfigMapPodTrafficManager, false)
	var cli *client.Client
	if cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation()); err != nil {
		return nil
	}
	var i types.NetworkResource
	if i, err = cli.NetworkInspect(ctx, config.ConfigMapPodTrafficManager, types.NetworkInspectOptions{}); err != nil {
		return nil
	}
	if len(i.Containers) == 0 {
		return cli.NetworkRemove(ctx, config.ConfigMapPodTrafficManager)
	}
	return nil
}
