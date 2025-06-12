package util

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func GetDNSServiceIPFromPod(ctx context.Context, clientset *kubernetes.Clientset, conf *rest.Config, podName, namespace string) (*dns.ClientConfig, error) {
	str, err := Shell(ctx, clientset, conf, podName, "", namespace, []string{"cat", "/etc/resolv.conf"})
	if err != nil {
		return nil, err
	}
	resolvConf, err := dns.ClientConfigFromReader(bytes.NewBufferString(strings.TrimSpace(str)))
	if err != nil {
		return nil, err
	}
	if resolvConf.Port == "" {
		resolvConf.Port = strconv.Itoa(53)
	}
	return resolvConf, nil
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
		err = errors.New("can not found any DNS service IP")
		return
	}
	err = nil
	return
}

func GetDNS(ctx context.Context, clientSet *kubernetes.Clientset, restConfig *rest.Config, ns, pod string) (*dns.ClientConfig, error) {
	_, err := clientSet.CoreV1().Pods(ns).Get(ctx, pod, v12.GetOptions{})
	if err != nil {
		return nil, err
	}
	clientConfig, err := GetDNSServiceIPFromPod(ctx, clientSet, restConfig, pod, ns)
	if err != nil {
		return nil, err
	}
	return clientConfig, nil
}
