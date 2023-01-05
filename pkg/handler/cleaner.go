package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	log "github.com/sirupsen/logrus"
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
		cleanupIfRefCountIsZero(clientset, namespace, config.ConfigMapPodTrafficManager)
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
func updateRefCount(configMapInterface v12.ConfigMapInterface, name string, increment int) {
	err := retry.OnError(
		retry.DefaultRetry,
		func(err error) bool { return !k8serrors.IsNotFound(err) },
		func() error {
			cm, err := configMapInterface.Get(context.Background(), name, v1.GetOptions{})
			if err != nil {
				log.Errorf("update ref-count failed, increment: %d, error: %v", increment, err)
				return err
			}
			curCount := 0
			if ref := cm.GetAnnotations()[config.AnnoRefCount]; len(ref) > 0 {
				curCount, err = strconv.Atoi(ref)
			}
			p, _ := json.Marshal([]interface{}{
				map[string]interface{}{
					"op":    "replace",
					"path":  fmt.Sprintf("/metadata/annotations/%s", config.AnnoRefCount),
					"value": strconv.Itoa(curCount + increment),
				},
			})
			_, err = configMapInterface.Patch(context.Background(), name, types.JSONPatchType, p, v1.PatchOptions{})
			return err
		})
	if err != nil {
		log.Errorf("update ref count error, error: %v", err)
	} else {
		log.Info("update ref count successfully")
	}
}

func cleanupIfRefCountIsZero(clientset *kubernetes.Clientset, namespace, name string) {
	updateRefCount(clientset.CoreV1().ConfigMaps(namespace), name, -1)
	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, v1.GetOptions{})
	if err != nil {
		log.Error(err)
		return
	}
	refCount, err := strconv.Atoi(cm.GetAnnotations()[config.AnnoRefCount])
	if err != nil {
		log.Error(err)
		return
	}
	// if ref-count is less than zero or equals to zero, means nobody is using this traffic pod, so clean it
	if refCount <= 0 {
		log.Info("refCount is zero, prepare to clean up resource")
		// keep configmap
		p := []byte(
			fmt.Sprintf(`[{"op": "remove", "path": "/data/%s"}]`, config.KeyDHCP),
		)
		_, err = clientset.CoreV1().ConfigMaps(namespace).Patch(context.Background(), name, types.JSONPatchType, p, v1.PatchOptions{})
		p = []byte(
			fmt.Sprintf(`[{"op": "replace", "path": "/metadata/annotations/%s", "value": "0"}]`, config.AnnoRefCount),
		)
		_, err = clientset.CoreV1().ConfigMaps(namespace).Patch(context.Background(), name, types.JSONPatchType, p, v1.PatchOptions{})
		options := v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
		_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.Background(), name+"."+namespace, options)
		_ = clientset.RbacV1().RoleBindings(namespace).Delete(context.Background(), name, options)
		_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(context.Background(), name, options)
		_ = clientset.RbacV1().Roles(namespace).Delete(context.Background(), name, options)
		_ = clientset.CoreV1().Services(namespace).Delete(context.Background(), name, options)
		_ = clientset.AppsV1().Deployments(namespace).Delete(context.Background(), name, options)
	}
}
