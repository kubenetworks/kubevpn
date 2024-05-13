package handler

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func (c *ConnectOptions) addCleanUpResourceHandler() {
	var stopChan = make(chan os.Signal)
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	go func() {
		select {
		case <-stopChan:
			c.Cleanup()
		case <-c.ctx.Done():
		}
	}()
}

func (c *ConnectOptions) Cleanup() {
	if c == nil {
		return
	}
	log.Info("prepare to exit, cleaning up")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	var ips []net.IP
	if c.localTunIPv4 != nil && c.localTunIPv4.IP != nil {
		ips = append(ips, c.localTunIPv4.IP)
	}
	if c.localTunIPv6 != nil && c.localTunIPv6.IP != nil {
		ips = append(ips, c.localTunIPv6.IP)
	}
	if c.dhcp != nil {
		err := c.dhcp.ReleaseIP(ctx, ips...)
		if err != nil {
			log.Errorf("failed to release ip to dhcp, err: %v", err)
		}
	}
	if c.clientset != nil {
		_ = c.clientset.CoreV1().Pods(c.Namespace).Delete(ctx, config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		count, err := updateRefCount(ctx, c.clientset.CoreV1().ConfigMaps(c.Namespace), config.ConfigMapPodTrafficManager, -1)
		// only if ref is zero and deployment is not ready, needs to clean up
		if err == nil && count <= 0 {
			deployment, errs := c.clientset.AppsV1().Deployments(c.Namespace).Get(ctx, config.ConfigMapPodTrafficManager, v1.GetOptions{})
			if errs == nil && deployment.Status.UnavailableReplicas != 0 {
				cleanupK8sResource(ctx, c.clientset, c.Namespace, config.ConfigMapPodTrafficManager, true)
			}
		}
		if err != nil {
			log.Errorf("can not update ref-count: %v", err)
		}
	}
	// leave proxy resources
	err := c.LeaveProxyResources(ctx)
	if err != nil {
		log.Errorf("leave proxy resources error: %v", err)
	}

	for _, function := range c.getRolloutFunc() {
		if function != nil {
			if err := function(); err != nil {
				log.Warningf("rollout function error: %v", err)
			}
		}
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.dnsConfig != nil {
		log.Infof("clean up dns")
		c.dnsConfig.CancelDNS()
	}
	log.Info("clean up successfully")
}

// vendor/k8s.io/kubectl/pkg/polymorphichelpers/rollback.go:99
func updateRefCount(ctx context.Context, configMapInterface v12.ConfigMapInterface, name string, increment int) (current int, err error) {
	err = retry.OnError(
		retry.DefaultRetry,
		func(err error) bool {
			notFound := k8serrors.IsNotFound(err)
			if notFound {
				return false
			}
			conflict := k8serrors.IsConflict(err)
			if conflict {
				return true
			}
			return false
		},
		func() (err error) {
			var cm *corev1.ConfigMap
			cm, err = configMapInterface.Get(ctx, name, v1.GetOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					return err
				}
				err = fmt.Errorf("update ref-count failed, increment: %d, error: %v", increment, err)
				return
			}
			curCount, _ := strconv.Atoi(cm.Data[config.KeyRefCount])
			var newVal = curCount + increment
			if newVal < 0 {
				newVal = 0
			}
			p := []byte(fmt.Sprintf(`{"data":{"%s":"%s"}}`, config.KeyRefCount, strconv.Itoa(newVal)))
			_, err = configMapInterface.Patch(ctx, name, types.MergePatchType, p, v1.PatchOptions{})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					return err
				}
				err = fmt.Errorf("update ref count error, error: %v", err)
				return
			}
			return
		})
	if err != nil {
		log.Errorf("update ref count error, increment: %d, error: %v", increment, err)
		return
	}
	log.Info("update ref count successfully")
	var cm *corev1.ConfigMap
	cm, err = configMapInterface.Get(ctx, name, v1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("failed to get cm: %s, err: %v", name, err)
		return
	}
	current, err = strconv.Atoi(cm.Data[config.KeyRefCount])
	if err != nil {
		err = fmt.Errorf("failed to get ref-count, err: %v", err)
	}
	return
}

func cleanupK8sResource(ctx context.Context, clientset *kubernetes.Clientset, namespace, name string, keepCIDR bool) {
	options := v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}

	if keepCIDR {
		// keep configmap
		p := []byte(fmt.Sprintf(`[{"op": "remove", "path": "/data/%s"},{"op": "remove", "path": "/data/%s"}]`, config.KeyDHCP, config.KeyDHCP6))
		_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(ctx, name, types.JSONPatchType, p, v1.PatchOptions{})
		p = []byte(fmt.Sprintf(`{"data":{"%s":"%s"}}`, config.KeyRefCount, strconv.Itoa(0)))
		_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(ctx, name, types.MergePatchType, p, v1.PatchOptions{})
	} else {
		_ = clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, options)
	}

	_ = clientset.CoreV1().Pods(namespace).Delete(ctx, config.CniNetName, options)
	_ = clientset.CoreV1().Secrets(namespace).Delete(ctx, name, options)
	_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, name+"."+namespace, options)
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, name, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(ctx, name, options)
	_ = clientset.CoreV1().Services(namespace).Delete(ctx, name, options)
	_ = clientset.AppsV1().Deployments(namespace).Delete(ctx, name, options)
}
