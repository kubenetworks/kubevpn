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

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
)

// GetDNSServiceIPFromPod returns the cluster DNS client configuration (servers, search
// domains, port). It prefers the manager-published resolv.conf cached in the traffic-manager
// ConfigMap (KeyClusterDNS) to avoid an exec, and falls back to exec-ing `cat /etc/resolv.conf`
// in the pod when the cache is absent (old manager) or unparseable. See docs/46.
func GetDNSServiceIPFromPod(ctx context.Context, clientset kubernetes.Interface, conf *rest.Config, podName, namespace string) (*dns.ClientConfig, error) {
	if cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, config.ConfigMapPodTrafficManager, v12.GetOptions{}); err == nil {
		if raw := strings.TrimSpace(cm.Data[config.KeyClusterDNS]); raw != "" {
			if resolvConf, err := dns.ClientConfigFromReader(bytes.NewBufferString(raw)); err == nil {
				if resolvConf.Port == "" {
					resolvConf.Port = strconv.Itoa(53)
				}
				return resolvConf, nil
			}
		}
	}

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
	return GetDNSServiceIPFromPod(ctx, clientSet, restConfig, pod, ns)
}
