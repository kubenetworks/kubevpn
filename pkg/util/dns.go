package util

import (
	"bytes"
	"context"

	"github.com/miekg/dns"
	v1 "k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

func GetDNSServiceIPFromPod(clientset *kubernetes.Clientset, restclient *rest.RESTClient, config *rest.Config, podName, namespace string) (*dns.ClientConfig, error) {
	resolvConfStr, err := Shell(clientset, restclient, config, podName, "", namespace, []string{"cat", "/etc/resolv.conf"})
	if err != nil {
		err = errors.Wrap(err, "Shell(clientset, restclient, config, podName, \"\", namespace, []string{\"cat\", \"/etc/resolv.conf\"}): ")
		return nil, err
	}
	resolvConf, err := dns.ClientConfigFromReader(bytes.NewBufferString(resolvConfStr))
	if err != nil {
		err = errors.Wrap(err, "dns.ClientConfigFromReader(bytes.NewBufferString(resolvConfStr)): ")
		return nil, err
	}
	if ips, err := GetDNSIPFromDnsPod(clientset); err == nil && len(ips) != 0 {
		resolvConf.Servers = ips
	}

	// linux nameserver only support amount is 3, so if namespace too much, just use two, left one to system
	if len(resolvConf.Servers) > 2 {
		resolvConf.Servers = resolvConf.Servers[:2]
	}

	return resolvConf, nil
}

func GetDNSIPFromDnsPod(clientset *kubernetes.Clientset) (ips []string, err error) {
	var serviceList *v1.ServiceList
	serviceList, err = clientset.CoreV1().Services(v12.NamespaceSystem).List(context.Background(), v12.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("k8s-app", "kube-dns").String(),
	})
	if err != nil {
		return
	}
	for _, item := range serviceList.Items {
		if len(item.Spec.ClusterIP) != 0 {
			ips = append(ips, item.Spec.ClusterIP)
		}
	}
	var podList *v1.PodList
	podList, err = clientset.CoreV1().Pods(v12.NamespaceSystem).List(context.Background(), v12.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("k8s-app", "kube-dns").String(),
	})
	if err == nil {
		for _, pod := range podList.Items {
			if pod.Status.Phase == v1.PodRunning && pod.DeletionTimestamp == nil {
				ips = append(ips, pod.Status.PodIP)
			}
		}
	}
	if len(ips) == 0 {
		err = errors.New("can not found any dns service ip")
		return
	}
	err = nil
	return
}
