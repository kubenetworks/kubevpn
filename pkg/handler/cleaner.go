package handler

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/dns"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

var stopChan = make(chan os.Signal)
var RollbackFuncList = make([]func(), 2)
var ctx, cancel = context.WithCancel(context.Background())

func (c *ConnectOptions) addCleanUpResourceHandler() {
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)
	go func() {
		<-stopChan
		log.Info("prepare to exit, cleaning up")
		err := c.dhcp.ReleaseIP(c.localTunIPv4.IP, c.localTunIPv6.IP)
		if err != nil {
			log.Errorf("failed to release ip to dhcp, err: %v", err)
		}
		for _, function := range RollbackFuncList {
			if function != nil {
				function()
			}
		}
		_ = c.clientset.CoreV1().Pods(c.Namespace).Delete(context.Background(), config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		var count int
		count, err = updateRefCount(c.clientset.CoreV1().ConfigMaps(c.Namespace), config.ConfigMapPodTrafficManager, -1)
		if err == nil {
			// only if ref is zero and deployment is not ready, needs to clean up
			if count <= 0 {
				deployment, errs := c.clientset.AppsV1().Deployments(c.Namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, v1.GetOptions{})
				if errs == nil && deployment.Status.UnavailableReplicas != 0 {
					cleanup(c.clientset, c.Namespace, config.ConfigMapPodTrafficManager, true)
				}
			}
		}
		if err != nil {
			log.Errorf("can not update ref-count: %v", err)
		}
		dns.CancelDNS()
		cancel()
		log.Info("clean up successful")
		util.CleanExtensionLib()
		os.Exit(0)
	}()
}

func Cleanup(s os.Signal) {
	select {
	case stopChan <- s:
	default:
	}
}

// vendor/k8s.io/kubectl/pkg/polymorphichelpers/rollback.go:99
func updateRefCount(configMapInterface v12.ConfigMapInterface, name string, increment int) (current int, err error) {
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
			cm, err = configMapInterface.Get(context.Background(), name, v1.GetOptions{})
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
			_, err = configMapInterface.Patch(context.Background(), name, types.MergePatchType, p, v1.PatchOptions{})
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
		return
	}
	log.Info("update ref count successfully")
	var cm *corev1.ConfigMap
	cm, err = configMapInterface.Get(context.Background(), name, v1.GetOptions{})
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

func cleanup(clientset *kubernetes.Clientset, namespace, name string, keepCIDR bool) {
	options := v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
	ctx1 := context.Background()

	if keepCIDR {
		// keep configmap
		p := []byte(fmt.Sprintf(`[{"op": "remove", "path": "/data/%s"},{"op": "remove", "path": "/data/%s"}]`, config.KeyDHCP, config.KeyDHCP6))
		_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(ctx1, name, types.JSONPatchType, p, v1.PatchOptions{})
		p = []byte(fmt.Sprintf(`{"data":{"%s":"%s"}}`, config.KeyRefCount, strconv.Itoa(0)))
		_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(ctx1, name, types.MergePatchType, p, v1.PatchOptions{})
	} else {
		_ = clientset.CoreV1().ConfigMaps(namespace).Delete(ctx1, name, options)
	}

	_ = clientset.CoreV1().Pods(namespace).Delete(ctx1, config.CniNetName, options)
	_ = clientset.CoreV1().Secrets(namespace).Delete(ctx1, name, options)
	_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx1, name+"."+namespace, options)
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(ctx1, name, options)
	_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx1, name, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(ctx1, name, options)
	_ = clientset.CoreV1().Services(namespace).Delete(ctx1, name, options)
	_ = clientset.AppsV1().Deployments(namespace).Delete(ctx1, name, options)
}
