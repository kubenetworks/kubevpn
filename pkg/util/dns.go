package util

import (
	"bytes"
	"context"
	"fmt"

	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

func GetDNSServiceIPFromPod(ctx context.Context, clientset *kubernetes.Clientset, restclient *rest.RESTClient, config *rest.Config, podName, namespace string) (*dns.ClientConfig, error) {
	str, err := Shell(ctx, clientset, restclient, config, podName, "", namespace, []string{"cat", "/etc/resolv.conf"})
	if err != nil {
		return nil, err
	}
	resolvConf, err := dns.ClientConfigFromReader(bytes.NewBufferString(str))
	if err == nil {
		return resolvConf, nil
	}

	ips, err := GetDNSIPFromDnsPod(ctx, clientset)
	if err != nil {
		return nil, err
	}
	clientConfig := dns.ClientConfig{
		Servers: ips,
		Search: []string{
			fmt.Sprintf("%s.svc.cluster.local", namespace),
			"svc.cluster.local",
			"cluster.local",
		},
		Ndots: 5,
	}
	return &clientConfig, nil
}

func GetDNSIPFromDnsPod(ctx context.Context, clientset *kubernetes.Clientset) (ips []string, err error) {
	var serviceList *v1.ServiceList
	serviceList, err = clientset.CoreV1().Services(v12.NamespaceSystem).List(ctx, v12.ListOptions{
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
	podList, err = clientset.CoreV1().Pods(v12.NamespaceSystem).List(ctx, v12.ListOptions{
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

func GetDNS(ctx context.Context, f util.Factory, ns, pod string) (*dns.ClientConfig, error) {
	clientSet, err := f.KubernetesClientSet()
	if err != nil {
		return nil, err
	}
	_, err = clientSet.CoreV1().Pods(ns).Get(ctx, pod, v12.GetOptions{})
	if err != nil {
		return nil, err
	}
	restConfig, err := f.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	client, err := f.RESTClient()
	if err != nil {
		return nil, err
	}

	clientConfig, err := GetDNSServiceIPFromPod(ctx, clientSet, client, restConfig, pod, ns)
	if err != nil {
		return nil, err
	}
	svc, err := clientSet.CoreV1().Services(ns).Get(ctx, config.ConfigMapPodTrafficManager, v12.GetOptions{})
	if err != nil {
		return nil, err
	}
	clientConfig.Servers = []string{svc.Spec.ClusterIP}
	return clientConfig, nil
}
