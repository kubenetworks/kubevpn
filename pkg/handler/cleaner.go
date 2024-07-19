package handler

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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

	c.once.Do(func() {
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
		}
		// leave proxy resources
		err := c.LeaveProxyResources(ctx)
		if err != nil {
			log.Errorf("leave proxy resources error: %v", err)
		}

		for _, function := range c.getRolloutFunc() {
			if function != nil {
				if err = function(); err != nil {
					log.Warningf("rollout function error: %v", err)
				}
			}
		}
		if c.cancel != nil {
			c.cancel()
		}
		if c.dnsConfig != nil {
			log.Infof("cleanup dns")
			c.dnsConfig.CancelDNS()
		}
	})
}

func cleanupK8sResource(ctx context.Context, clientset *kubernetes.Clientset, namespace, name string, keepCIDR bool) {
	options := v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}

	if keepCIDR {
		// keep configmap
		p := []byte(fmt.Sprintf(`[{"op": "remove", "path": "/data/%s"},{"op": "remove", "path": "/data/%s"}]`, config.KeyDHCP, config.KeyDHCP6))
		_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(ctx, name, types.JSONPatchType, p, v1.PatchOptions{})
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
