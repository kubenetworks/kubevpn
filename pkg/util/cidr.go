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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetCIDRElegant
// 1) dump cluster info
// 2) grep cmdline
// 3) create svc + cat *.conflist
// 4) create svc + get pod ip with svc mask
func GetCIDRElegant(ctx context.Context, clientset *kubernetes.Clientset, restconfig *rest.Config, namespace string) ([]*net.IPNet, error) {
	defer func() {
		_ = clientset.CoreV1().Pods(namespace).Delete(context.Background(), config.CniNetName, v1.DeleteOptions{GracePeriodSeconds: pointer.Int64(0)})
	}()

	var result []*net.IPNet
	log.Infoln("get cidr from cluster info...")
	info, err := GetCIDRByDumpClusterInfo(ctx, clientset)
	if err == nil {
		log.Infoln("get cidr from cluster info ok")
		result = append(result, info...)
	}

	log.Infoln("get cidr from cni...")
	cni, err := GetCIDRFromCNI(ctx, clientset, restconfig, namespace)
	if err == nil {
		log.Infoln("get cidr from cni ok")
		result = append(result, cni...)
	}

	pod, err := GetPodCIDRFromCNI(ctx, clientset, restconfig, namespace)
	if err == nil {
		result = append(result, pod...)
	}

	svc, err := GetServiceCIDRByCreateService(ctx, clientset.CoreV1().Services(namespace))
	if err == nil {
		result = append(result, svc)
	}

	log.Infoln("get cidr from svc...")
	pod, err = GetPodCIDRFromPod(ctx, clientset, namespace, svc)
	if err == nil {
		log.Infoln("get cidr from svc ok")
		result = append(result, pod...)
	}

	result = Deduplicate(result)
	if len(result) == 0 {
		err = fmt.Errorf("can not get any cidr, please make sure you have prilivage")
		return nil, err
	}
	return result, nil
}

// GetCIDRFromResourceUgly
// use podIP/24 and serviceIP/24 as cidr
func GetCIDRFromResourceUgly(ctx context.Context, clientset *kubernetes.Clientset, namespace string) []*net.IPNet {
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
	for _, n := range []string{v1.NamespaceAll, namespace} {
		podList, err := clientset.CoreV1().Pods(n).List(ctx, v1.ListOptions{})
		if err != nil {
			continue
		}
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
		break
	}

	// (2) get service CIDR
	for _, n := range []string{v1.NamespaceAll, namespace} {
		serviceList, err := clientset.CoreV1().Services(n).List(ctx, v1.ListOptions{})
		if err != nil {
			continue
		}
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
		break
	}

	return cidrs
}

func GetLocalTunIP(tunName string) (net.IP, net.IP, error) {
	tunIface, err := net.InterfaceByName(tunName)
	if err != nil {
		return nil, nil, err
	}
	addrs, err := tunIface.Addrs()
	if err != nil {
		return nil, nil, err
	}
	var srcIPv4, srcIPv6 net.IP
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ip.To4() != nil {
			srcIPv4 = ip
		} else {
			srcIPv6 = ip
		}
	}
	if srcIPv4 == nil || srcIPv6 == nil {
		return srcIPv4, srcIPv6, fmt.Errorf("not found all ip")
	}
	return srcIPv4, srcIPv6, nil
}
