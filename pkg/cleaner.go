package pkg

import (
	"context"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"github.com/wencaiwulue/kubevpn/dns"
	"github.com/wencaiwulue/kubevpn/util"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

var stopChan = make(chan os.Signal)
var rollbackFuncList = make([]func(), 2)
var ctx, cancel = context.WithCancel(context.TODO())

func AddCleanUpResourceHandler(clientset *kubernetes.Clientset, namespace string, manager *DHCPManager, ip ...*net.IPNet) {
	signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
	go func() {
		<-stopChan
		log.Info("prepare to exit, cleaning up")
		dns.CancelDNS()
		for _, ipNet := range ip {
			if err := manager.ReleaseIpToDHCP(ipNet); err != nil {
				log.Errorf("failed to release ip to dhcp, err: %v", err)
			}
		}
		cleanUpTrafficManagerIfRefCountIsZero(clientset, namespace)
		cancel()
		for _, function := range rollbackFuncList {
			if function != nil {
				function()
			}
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
func UpdateRefCount(clientset *kubernetes.Clientset, namespace, name string, increment int) {
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return !k8serrors.IsNotFound(err)
	}, func() error {
		pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), name, v1.GetOptions{})
		if err != nil {
			log.Errorf("update ref-count failed, increment: %d, error: %v", increment, err)
			return err
		}
		curCount := 0
		if ref := pod.GetAnnotations()["ref-count"]; len(ref) > 0 {
			curCount, err = strconv.Atoi(ref)
		}
		patch, _ := json.Marshal([]interface{}{
			map[string]interface{}{
				"op":    "replace",
				"path":  "/metadata/annotations/ref-count",
				"value": strconv.Itoa(curCount + increment),
			},
		})
		_, err = clientset.CoreV1().Pods(namespace).
			Patch(context.TODO(), util.TrafficManager, types.JSONPatchType, patch, v1.PatchOptions{})
		return err
	}); err != nil {
		log.Errorf("update ref count error, error: %v", err)
	} else {
		log.Info("update ref count successfully")
	}
}

func cleanUpTrafficManagerIfRefCountIsZero(clientset *kubernetes.Clientset, namespace string) {
	UpdateRefCount(clientset, namespace, util.TrafficManager, -1)
	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), util.TrafficManager, v1.GetOptions{})
	if err != nil {
		log.Error(err)
		return
	}
	refCount, err := strconv.Atoi(pod.GetAnnotations()["ref-count"])
	if err != nil {
		log.Error(err)
		return
	}
	// if refcount is less than zero or equals to zero, means no body will using this dns pod, so clean it
	if refCount <= 0 {
		zero := int64(0)
		log.Info("refCount is zero, prepare to clean up resource")
		_ = clientset.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), util.TrafficManager, v1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		_ = clientset.CoreV1().Pods(namespace).Delete(context.TODO(), util.TrafficManager, v1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
	}
}
