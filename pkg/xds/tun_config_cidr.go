package xds

import (
	"context"
	"net"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// WarmClusterCIDRCache detects the cluster CIDRs in-cluster (once, best-effort) and
// writes them to the traffic-manager ConfigMap's CLUSTER_CIDRS key, so connecting
// clients read the cache instead of each running the probe-pod/exec detection.
// See docs/46.
//
// It is strictly additive: it only fills an EMPTY cache and never overwrites, so a
// client (or a manual edit) that populated it wins; on any failure the cache stays
// empty and clients fall back to local detection — i.e. the pre-existing behavior.
// The detected set is stored RAW (deduped, unfiltered): every reader filters by its
// own API-server IPs via handler.parseCachedCIDRs.
func (s *TunConfigServer) WarmClusterCIDRCache(ctx context.Context) {
	// Detect WITHOUT creating a probe pod / exec: the manager is already in-cluster,
	// so it infers CIDRs from kube-system component flags, a rejected Service create,
	// and existing pod IPs — no pods/create or pods/exec RBAC needed. See docs/46.
	s.warmClusterCIDRCache(ctx, func() []*net.IPNet {
		return util.GetClusterCIDRNoProbePod(ctx, s.clientset, s.namespace)
	})
}

// warmClusterCIDRCache is the testable core; detect is injected so the ConfigMap
// skip/write logic can be exercised without the real (exec/probe-pod) detector.
func (s *TunConfigServer) warmClusterCIDRCache(ctx context.Context, detect func() []*net.IPNet) {
	defer func() {
		if r := recover(); r != nil {
			plog.G(ctx).Errorf("[TunConfig] CIDR warm-up panicked (ignored): %v", r)
		}
	}()

	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: get ConfigMap: %v", err)
		return
	}
	if strings.TrimSpace(cm.Data[config.KeyClusterCIDRs]) != "" {
		return // already populated — never overwrite
	}

	deduped := util.RemoveLargerOverlappingCIDRs(detect())
	if len(deduped) == 0 {
		plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: no CIDRs detected; clients fall back to local detection")
		return
	}
	encoded := encodeCIDRSet(deduped)

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, config.ConfigMapPodTrafficManager, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if strings.TrimSpace(cm.Data[config.KeyClusterCIDRs]) != "" {
			return nil // a client filled it in the meantime — leave it
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[config.KeyClusterCIDRs] = encoded
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: write cache: %v", err)
		return
	}
	plog.G(ctx).Infof("[TunConfig] warmed cluster CIDR cache (raw): %s", encoded)
}

// encodeCIDRSet serializes CIDRs into a deduplicated space-separated string, matching
// the CLUSTER_CIDRS ConfigMap format read by the client (handler.parseCachedCIDRs).
func encodeCIDRSet(cidrs []*net.IPNet) string {
	set := sets.New[string]()
	for _, c := range cidrs {
		set.Insert(c.String())
	}
	return strings.Join(set.UnsortedList(), " ")
}
