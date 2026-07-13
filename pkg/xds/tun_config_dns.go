package xds

import (
	"context"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

const managerResolvConfPath = "/etc/resolv.conf"

// WarmClusterDNSCache publishes the manager pod's own /etc/resolv.conf (cluster DNS
// server + search domains) into the traffic-manager ConfigMap's CLUSTER_DNS_RESOLV
// key, so clients read it from the cache instead of exec-ing `cat /etc/resolv.conf`
// into the pod. See docs/46. Reading the pod's own resolv.conf is a local file read
// and needs no extra RBAC (the manager already owns its ConfigMap).
//
// Additive: only fills an EMPTY key, never overwrites; on any failure the key stays
// empty and clients fall back to exec. Per-client namespace enumeration and the
// ClusterIP-vs-PodIP connectivity probe stay on the client.
func (s *TunConfigServer) WarmClusterDNSCache(ctx context.Context) {
	s.warmClusterDNSCache(ctx, func() (string, error) {
		b, err := os.ReadFile(managerResolvConfPath)
		return string(b), err
	})
}

// warmClusterDNSCache is the testable core; readResolv is injected so the ConfigMap
// skip/write logic can be tested without touching the real /etc/resolv.conf.
func (s *TunConfigServer) warmClusterDNSCache(ctx context.Context, readResolv func() (string, error)) {
	defer func() {
		if r := recover(); r != nil {
			plog.G(ctx).Errorf("[TunConfig] DNS warm-up panicked (ignored): %v", r)
		}
	}()

	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] DNS warm-up: get ConfigMap: %v", err)
		return
	}
	if strings.TrimSpace(cm.Data[config.KeyClusterDNS]) != "" {
		return // already populated — never overwrite
	}

	raw, err := readResolv()
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] DNS warm-up: read resolv.conf: %v", err)
		return
	}
	if strings.TrimSpace(raw) == "" {
		plog.G(ctx).Warnf("[TunConfig] DNS warm-up: empty resolv.conf; clients fall back to exec")
		return
	}

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if strings.TrimSpace(cm.Data[config.KeyClusterDNS]) != "" {
			return nil // filled in the meantime — leave it
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[config.KeyClusterDNS] = raw
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] DNS warm-up: write cache: %v", err)
		return
	}
	plog.G(ctx).Infof("[TunConfig] warmed cluster DNS cache from %s", managerResolvConfPath)
}
