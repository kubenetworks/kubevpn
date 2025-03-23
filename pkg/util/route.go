package util

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"

	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

func GetNsForListPodAndSvc(ctx context.Context, clientset *kubernetes.Clientset, nsList []string) (podNs string, svcNs string, err error) {
	for _, ns := range nsList {
		_, err = clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if apierrors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return
		}
		podNs = ns
		break
	}
	if err != nil {
		err = errors.Wrap(err, "can not list pod to add it to route table")
		return
	}
	if podNs == "" {
		plog.G(ctx).Debugf("List all namepsace pods")
	} else {
		plog.G(ctx).Debugf("List namepsace %s pods", podNs)
	}

	for _, ns := range nsList {
		_, err = clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if apierrors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return
		}
		svcNs = ns
		break
	}
	if err != nil {
		err = errors.Wrap(err, "can not list service to add it to route table")
		return
	}
	if svcNs == "" {
		plog.G(ctx).Debugf("List all namepsace services")
	} else {
		plog.G(ctx).Debugf("List namepsace %s services", svcNs)
	}
	return
}

func ListService(ctx context.Context, lister v12.ServiceInterface, addRouteFunc func(ipStr ...string) error) error {
	opts := metav1.ListOptions{Limit: 100, Continue: ""}
	for {
		serviceList, err := lister.List(ctx, opts)
		if err != nil {
			return err
		}
		var ips []string
		for _, service := range serviceList.Items {
			ips = append(ips, service.Spec.ClusterIP)
		}
		err = addRouteFunc(ips...)
		if err != nil {
			plog.G(ctx).Errorf("Failed to add service IP to route table: %v", err)
		}
		if serviceList.Continue == "" {
			return nil
		}
		opts.Continue = serviceList.Continue
	}
}

func WatchServiceToAddRoute(ctx context.Context, watcher v12.ServiceInterface, routeFunc func(ipStr ...string) error) error {
	defer func() {
		if er := recover(); er != nil {
			plog.G(ctx).Error(er)
		}
	}()
	w, err := watcher.Watch(ctx, metav1.ListOptions{Watch: true})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-w.ResultChan():
			if !ok {
				return errors.New("watch service chan done")
			}
			var svc *v1.Service
			svc, ok = e.Object.(*v1.Service)
			if !ok {
				continue
			}
			_ = routeFunc(svc.Spec.ClusterIP)
		}
	}
}

func ListPod(ctx context.Context, lister v12.PodInterface, addRouteFunc func(ipStr ...string) error) error {
	opts := metav1.ListOptions{Limit: 100, Continue: ""}
	for {
		podList, err := lister.List(ctx, opts)
		if err != nil {
			return err
		}
		var ips []string
		for _, pod := range podList.Items {
			if pod.Spec.HostNetwork {
				continue
			}
			ips = append(ips, pod.Status.PodIP)
		}
		err = addRouteFunc(ips...)
		if err != nil {
			plog.G(ctx).Errorf("Failed to add Pod IP to route table: %v", err)
		}
		if podList.Continue == "" {
			return nil
		}
		opts.Continue = podList.Continue
	}
}

func WatchPodToAddRoute(ctx context.Context, watcher v12.PodInterface, addRouteFunc func(ipStrList ...string) error) error {
	defer func() {
		if er := recover(); er != nil {
			plog.G(ctx).Errorln(er)
		}
	}()
	w, err := watcher.Watch(ctx, metav1.ListOptions{Watch: true})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("watch pod chan done")
			}
			var pod *v1.Pod
			pod, ok = e.Object.(*v1.Pod)
			if !ok {
				continue
			}
			if pod.Spec.HostNetwork {
				continue
			}
			ip := pod.Status.PodIP
			_ = addRouteFunc(ip)
		}
	}
}
