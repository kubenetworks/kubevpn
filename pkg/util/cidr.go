package util

import (
	"context"
	"fmt"
	"net"

	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
)

// GetCIDRElegant
// 1) dump cluster info
// 2) grep cmdline
// 3) create svc + cat *.conflist
// 4) create svc + get pod ip with svc mask
func GetCIDRElegant(clientset *kubernetes.Clientset, restclient *rest.RESTClient, restconfig *rest.Config, namespace string) (result []*net.IPNet, err1 error) {
	defer func() {
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
	}()

	log.Infoln("get cidr from cluster info...")
	info, err := getCIDRByDumpClusterInfo(clientset)
	if err == nil {
		log.Infoln("get cidr from cluster info ok")
		result = append(result, info...)
	}

	log.Infoln("get cidr from cni...")
	cni, err := getCIDRFromCNI(clientset, restclient, restconfig, namespace)
	if err == nil {
		log.Infoln("get cidr from cni ok")
		result = append(result, cni...)
	}

	pod, err := getPodCIDRFromCNI(clientset, restclient, restconfig, namespace)
	if err == nil {
		result = append(result, pod...)
	}

	svc, err := getServiceCIDRByCreateSvc(clientset.CoreV1().Services(namespace))
	if err == nil {
		result = append(result, svc)
	}

	log.Infoln("get cidr from svc...")
	pod, err = getPodCIDRFromPod(clientset, namespace, svc)
	if err == nil {
		log.Infoln("get cidr from svc ok")
		result = append(result, pod...)
	}

	result = Deduplicate(result)

	if len(result) == 0 {
		err = fmt.Errorf("can not get any cidr, please make sure you have prilivage")
		return
	}
	return
}

// GetCIDRFromResourceUgly
// use podIP/24 and serviceIP/16 as cidr
func GetCIDRFromResourceUgly(clientset *kubernetes.Clientset, namespace string) ([]*net.IPNet, error) {
	var cidrs []*net.IPNet
	// (2) get pod CIDR from pod ip, why doing this: notice that node's pod cidr is not correct in minikube
	// ➜  ~ kubectl get nodes -o jsonpath='{.items[*].spec.podCIDR}'
	//10.244.0.0/24%
	// ➜  ~  kubectl get pods -o=custom-columns=podIP:.status.podIP
	//podIP
	//172.17.0.5
	//172.17.0.4
	//172.17.0.4
	//172.17.0.3
	//172.17.0.3
	//172.17.0.6
	//172.17.0.8
	//172.17.0.3
	//172.17.0.7
	//172.17.0.2
	podList, _ := clientset.CoreV1().Pods(namespace).List(context.Background(), v1.ListOptions{})
	for _, pod := range podList.Items {
		if pod.Spec.HostNetwork {
			continue
		}
		s := sets.Set[string]{}.Insert(pod.Status.PodIP)
		for _, p := range pod.Status.PodIPs {
			s.Insert(p.IP)
		}
		for _, t := range s.UnsortedList() {
			if ip := net.ParseIP(t); ip != nil {
				var mask net.IPMask
				if ip.To4() != nil {
					mask = net.CIDRMask(24, 32)
				} else {
					mask = net.CIDRMask(64, 128)
				}
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}

	// (2) get service CIDR
	serviceList, _ := clientset.CoreV1().Services(namespace).List(context.Background(), v1.ListOptions{})
	for _, service := range serviceList.Items {
		s := sets.Set[string]{}.Insert(service.Spec.ClusterIP)
		for _, p := range service.Spec.ClusterIPs {
			s.Insert(p)
		}
		for _, t := range s.UnsortedList() {
			if ip := net.ParseIP(t); ip != nil {
				var mask net.IPMask
				if ip.To4() != nil {
					mask = net.CIDRMask(24, 32)
				} else {
					mask = net.CIDRMask(64, 128)
				}
				cidrs = append(cidrs, &net.IPNet{IP: ip.Mask(mask), Mask: mask})
			}
		}
	}

	return cidrs, nil
}
