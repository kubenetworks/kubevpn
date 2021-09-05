package remote

import (
	"context"
	"encoding/json"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"kubevpn/util"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var stopChan = make(chan os.Signal)

func AddCleanUpResourceHandler(client *kubernetes.Clientset, namespace string, services string, ip ...*net.IPNet) {
	signal.Notify(stopChan, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL /*, syscall.SIGSTOP*/)
	go func() {
		<-stopChan
		log.Info("prepare to exit, cleaning up")
		for _, ipNet := range ip {
			if err := ReleaseIpToDHCP(client, namespace, ipNet); err != nil {
				log.Errorf("failed to release ip to dhcp, err: %v", err)
			}
		}
		cleanUpTrafficManagerIfRefCountIsZero(client, namespace)
		wg := sync.WaitGroup{}
		for _, service := range strings.Split(services, ",") {
			if len(service) > 0 {
				wg.Add(1)
				go func(finalService string) {
					defer wg.Done()
					util.ScaleDeploymentReplicasTo(client, namespace, finalService, 1)
					newName := finalService + "-" + "shadow"
					deletePod(client, namespace, newName)
				}(service)
			}
		}
		wg.Wait()
		log.Info("clean up successful")
		os.Exit(0)
	}()
}

func deletePod(client *kubernetes.Clientset, namespace, podName string) {
	zero := int64(0)
	err := client.CoreV1().Pods(namespace).Delete(context.TODO(), podName, v1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
	if err != nil && errors.IsNotFound(err) {
		log.Infof("not found shadow pod: %s, no need to delete it", podName)
	}
}

// vendor/k8s.io/kubectl/pkg/polymorphichelpers/rollback.go:99
func updateRefCount(client *kubernetes.Clientset, namespace, name string, increment int) {
	if err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return err != nil
	}, func() error {
		pod, err := client.CoreV1().Pods(namespace).Get(context.TODO(), name, v1.GetOptions{})
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
		_, err = client.CoreV1().Pods(namespace).
			Patch(context.TODO(), util.TrafficManager, types.JSONPatchType, patch, v1.PatchOptions{})
		return err
	}); err != nil {
		log.Errorf("update ref count error, error: %v", err)
	} else {
		log.Info("update ref count successfully")
	}
}

func cleanUpTrafficManagerIfRefCountIsZero(client *kubernetes.Clientset, namespace string) {
	updateRefCount(client, namespace, util.TrafficManager, -1)
	pod, err := client.CoreV1().Pods(namespace).Get(context.TODO(), util.TrafficManager, v1.GetOptions{})
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
		_ = client.CoreV1().ConfigMaps(namespace).Delete(context.TODO(), util.TrafficManager, v1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		_ = client.CoreV1().Pods(namespace).Delete(context.TODO(), util.TrafficManager, v1.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
	}
}
