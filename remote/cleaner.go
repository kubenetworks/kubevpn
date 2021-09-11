package remote

import (
	"context"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"kubevpn/util"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
)

var stopChan = make(chan os.Signal)

func AddCleanUpResourceHandler(clientset *kubernetes.Clientset, namespace string, workloads []string, ip ...*net.IPNet) {
	signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
	go func() {
		<-stopChan
		log.Info("prepare to exit, cleaning up")
		for _, ipNet := range ip {
			if err := ReleaseIpToDHCP(clientset, namespace, ipNet); err != nil {
				log.Errorf("failed to release ip to dhcp, err: %v", err)
			}
		}
		cleanUpTrafficManagerIfRefCountIsZero(clientset, namespace)
		wg := sync.WaitGroup{}
		for _, service := range workloads {
			if len(service) > 0 {
				wg.Add(1)
				go func(finalService string) {
					defer wg.Done()
					newName := finalService + "-" + "shadow"
					util.DeletePod(clientset, namespace, newName)
				}(service)
			}
		}
		wg.Wait()
		wg = sync.WaitGroup{}
		for _, controller := range util.TopLevelControllerSet {
			wg.Add(1)
			go func(control util.ResourceTupleWithScale) {
				util.UpdateReplicasScale(clientset, namespace, control)
				wg.Done()
			}(controller)
		}
		wg.Wait()
		log.Info("clean up successful")
		os.Exit(0)
	}()
}

// vendor/k8s.io/kubectl/pkg/polymorphichelpers/rollback.go:99
func updateRefCount(clientset *kubernetes.Clientset, namespace, name string, increment int) {
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
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
	updateRefCount(clientset, namespace, util.TrafficManager, -1)
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
