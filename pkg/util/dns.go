package util

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// GetDNSServiceIPFromPod reads /etc/resolv.conf from the specified pod and returns the parsed DNS client configuration.
func GetDNSServiceIPFromPod(ctx context.Context, clientset kubernetes.Interface, conf *rest.Config, podName, namespace string) (*dns.ClientConfig, error) {
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

// GetDNS retrieves the DNS configuration from the specified pod after verifying the pod exists.
func GetDNS(ctx context.Context, clientSet kubernetes.Interface, restConfig *rest.Config, ns, pod string) (*dns.ClientConfig, error) {
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
