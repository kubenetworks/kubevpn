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
)

var stopChan = make(chan os.Signal)
var RollbackFuncList = make([]func(), 2)
var ctx, cancel = context.WithCancel(context.Background())

func (c *ConnectOptions) addCleanUpResourceHandler(clientset *kubernetes.Clientset, namespace string) {
	signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
	go func() {
		<-stopChan
		log.Info("prepare to exit, cleaning up")
		dns.CancelDNS()
		err := c.dhcp.ReleaseIpToDHCP(c.usedIPs...)
		if err != nil {
			log.Errorf("failed to release ip to dhcp, err: %v", err)
		}
		cancel()
		for _, function := range RollbackFuncList {
			if function != nil {
				function()
			}
		}
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
		var count int
		count, err = updateRefCount(clientset.CoreV1().ConfigMaps(namespace), config.ConfigMapPodTrafficManager, -1)
		if err == nil {
			// if ref-count is less than zero or equals to zero, means nobody is using this traffic pod, so clean it
			if count <= 0 {
				cleanup(clientset, namespace, config.ConfigMapPodTrafficManager)
			}
		} else {
			log.Error(err)
		}
		log.Info("clean up successful")
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

func cleanup(clientset *kubernetes.Clientset, namespace, name string) {
	log.Info("ref-count is zero, prepare to clean up resource")
	// keep configmap
	p := []byte(fmt.Sprintf(`[{"op": "remove", "path": "/data/%s"}]`, config.KeyDHCP))
	_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(context.Background(), name, types.JSONPatchType, p, v1.PatchOptions{})
	p = []byte(fmt.Sprintf(`{"data":{"%s":"%s"}}`, config.KeyRefCount, strconv.Itoa(0)))
	_, _ = clientset.CoreV1().ConfigMaps(namespace).Patch(context.Background(), name, types.MergePatchType, p, v1.PatchOptions{})
	options := v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
	_ = clientset.CoreV1().Secrets(namespace).Delete(context.Background(), name, options)
	_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.Background(), name+"."+namespace, options)
	_ = clientset.RbacV1().RoleBindings(namespace).Delete(context.Background(), name, options)
	_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(context.Background(), name, options)
	_ = clientset.RbacV1().Roles(namespace).Delete(context.Background(), name, options)
	_ = clientset.CoreV1().Services(namespace).Delete(context.Background(), name, options)
	_ = clientset.AppsV1().Deployments(namespace).Delete(context.Background(), name, options)
}
