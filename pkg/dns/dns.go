package dns

import (
	"bytes"
	"context"

	miekgdns "github.com/miekg/dns"
	"github.com/pkg/errors"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func GetDNSServiceIPFromPod(clientset *kubernetes.Clientset, restclient *rest.RESTClient, config *rest.Config, podName, namespace string) (*miekgdns.ClientConfig, error) {
	var ipp []string
	if ips, err := getDNSIPFromDnsPod(clientset); err == nil {
		ipp = ips
	}
	resolvConfStr, err := util.Shell(clientset, restclient, config, podName, namespace, "cat /etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	resolvConf, err := miekgdns.ClientConfigFromReader(bytes.NewBufferString(resolvConfStr))
	if err != nil {
		return nil, err
	}
	if len(ipp) != 0 {
		resolvConf.Servers = append(resolvConf.Servers, make([]string, len(ipp))...)
		copy(resolvConf.Servers[len(ipp):], resolvConf.Servers[:len(resolvConf.Servers)-len(ipp)])
		for i := range ipp {
			resolvConf.Servers[i] = ipp[i]
		}
	}
	return resolvConf, nil
}

func getDNSIPFromDnsPod(clientset *kubernetes.Clientset) (ips []string, err error) {
	var serviceList *v12.ServiceList
	serviceList, err = clientset.CoreV1().Services(v1.NamespaceSystem).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("k8s-app", "kube-dns").String(),
	})
	if err != nil {
		return nil, err
	}
	for _, item := range serviceList.Items {
		if len(item.Spec.ClusterIP) != 0 {
			ips = append(ips, item.Spec.ClusterIP)
		}
	}
	var podList *v12.PodList
	podList, err = clientset.CoreV1().Pods(v1.NamespaceSystem).List(context.Background(), v1.ListOptions{
		LabelSelector: fields.OneTermEqualSelector("k8s-app", "kube-dns").String(),
	})
	if err != nil {
		return
	}
	for _, pod := range podList.Items {
		if pod.Status.Phase == v12.PodRunning && pod.DeletionTimestamp == nil {
			ips = append(ips, pod.Status.PodIP)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("")
	}
	return ips, nil
}
