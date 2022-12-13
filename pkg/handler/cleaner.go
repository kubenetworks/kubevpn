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
		cleanUpTrafficManagerIfRefCountIsZero(clientset, namespace)
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
func updateServiceRefCount(serviceInterface v12.ServiceInterface, name string, increment int) {
	err := retry.OnError(
		retry.DefaultRetry,
		func(err error) bool { return !k8serrors.IsNotFound(err) },
		func() error {
			service, err := serviceInterface.Get(context.Background(), name, v1.GetOptions{})
			if err != nil {
				log.Errorf("update ref-count failed, increment: %d, error: %v", increment, err)
				return err
			}
			curCount := 0
			if ref := service.GetAnnotations()["ref-count"]; len(ref) > 0 {
				curCount, err = strconv.Atoi(ref)
			}
			p, _ := json.Marshal([]interface{}{
				map[string]interface{}{
					"op":    "replace",
					"path":  "/metadata/annotations/ref-count",
					"value": strconv.Itoa(curCount + increment),
				},
			})
			_, err = serviceInterface.Patch(context.Background(), config.ConfigMapPodTrafficManager, types.JSONPatchType, p, v1.PatchOptions{})
			return err
		})
	if err != nil {
		log.Errorf("update ref count error, error: %v", err)
	} else {
		log.Info("update ref count successfully")
	}
}

func cleanUpTrafficManagerIfRefCountIsZero(clientset *kubernetes.Clientset, namespace string) {
	updateServiceRefCount(clientset.CoreV1().Services(namespace), config.ConfigMapPodTrafficManager, -1)
	pod, err := clientset.CoreV1().Services(namespace).Get(context.Background(), config.ConfigMapPodTrafficManager, v1.GetOptions{})
	if err != nil {
		log.Error(err)
		return
	}
	refCount, err := strconv.Atoi(pod.GetAnnotations()["ref-count"])
	if err != nil {
		log.Error(err)
		return
	}
	// if refcount is less than zero or equals to zero, means nobody is using this traffic pod, so clean it
	if refCount <= 0 {
		log.Info("refCount is zero, prepare to clean up resource")
		// keep configmap
		p := []byte(fmt.Sprintf(`[{"op": "remove", "path": "/data/%s"}]`, config.KeyDHCP))
		_, err = clientset.CoreV1().ConfigMaps(namespace).Patch(context.Background(), config.ConfigMapPodTrafficManager, types.JSONPatchType, p, v1.PatchOptions{})
		deleteOptions := v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)}
		_ = clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(context.Background(), config.ConfigMapPodTrafficManager+"."+namespace, deleteOptions)
		_ = clientset.RbacV1().RoleBindings(namespace).Delete(context.Background(), config.ConfigMapPodTrafficManager, deleteOptions)
		_ = clientset.CoreV1().ServiceAccounts(namespace).Delete(context.Background(), config.ConfigMapPodTrafficManager, deleteOptions)
		_ = clientset.RbacV1().Roles(namespace).Delete(context.Background(), config.ConfigMapPodTrafficManager, deleteOptions)
		_ = clientset.CoreV1().Services(namespace).Delete(context.Background(), config.ConfigMapPodTrafficManager, deleteOptions)
		_ = clientset.AppsV1().Deployments(namespace).Delete(context.Background(), config.ConfigMapPodTrafficManager, deleteOptions)
	}
}
