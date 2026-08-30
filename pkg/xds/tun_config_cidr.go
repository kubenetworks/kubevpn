package xds

import (
	"context"
	"net"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// WarmClusterCIDRCache detects the cluster CIDRs in-cluster (once, best-effort) and
// writes them to the traffic-manager ConfigMap's CLUSTER_CIDRs key, so connecting
// clients read the cache instead of each running the probe-pod/exec detection.
// See docs/46.
//
// Cache contract (version-gated):
//   - Empty CIDR cache -> run detection, write RAW deduped CIDRs + stamp schema.
//   - Populated cache with a CURRENT schema -> skip (never overwrite; protects manual
//     edits and a client/operator-set value).
//   - Populated cache with an ABSENT or OLDER schema (written by a pre-versioning or
//     buggy manager/client, e.g. v2.11.6 under-detected GKE's Service CIDR) -> re-run
//     detection and OVERWRITE with the fresh result + stamp schema. This auto-recovers
//     a poisoned cache on manager upgrade; a non-empty detection is required (an empty
//     result never clobbers an existing value).
//
// The detected set is stored RAW (deduped, unfiltered): every reader filters by its
// own API-server IPs via handler.parseCachedCIDRs.
func (s *TunConfigServer) WarmClusterCIDRCache(ctx context.Context) {
	// Detect WITHOUT creating a probe pod / exec: the manager is already in-cluster,
	// so it infers CIDRs from kube-system component flags, a rejected (dry-run) Service
	// create, and existing pod IPs -- no pods/create or pods/exec RBAC needed. See docs/46.
	s.warmClusterCIDRCache(ctx, func() []*net.IPNet {
		return util.GetClusterCIDRNoProbePod(ctx, s.clientset, s.namespace)
	})
}

// warmClusterCIDRCache is the testable core; detect is injected so the ConfigMap
// skip/write/overwrite logic can be exercised without the real (exec/probe-pod) detector.
func (s *TunConfigServer) warmClusterCIDRCache(ctx context.Context, detect func() []*net.IPNet) {
	defer func() {
		if r := recover(); r != nil {
			plog.G(ctx).Errorf("[TunConfig] CIDR warm-up panicked (ignored): %v", r)
		}
	}()

	cidrVal, schemaVal, err := s.readClusterCIDRCache(ctx)
	if err != nil {
		plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: get ConfigMap: %v", err)
		return
	}

	cacheEmpty := strings.TrimSpace(cidrVal) == ""
	stale := !cacheIsCurrentSchema(schemaVal)

	// Current-schema cache is authoritative: never overwrite (protects manual edits).
	if !cacheEmpty && !stale {
		return
	}

	deduped := util.RemoveLargerOverlappingCIDRs(detect())
	if len(deduped) == 0 {
		if cacheEmpty {
			plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: no CIDRs detected; clients fall back to local detection")
		} else {
			plog.G(ctx).Infof("[TunConfig] CIDR warm-up: no CIDRs detected; leaving stale cache %q untouched", cidrVal)
		}
		return
	}
	encoded := encodeCIDRSet(deduped)

	if cacheEmpty {
		// Fill an empty key (never overwrites a concurrent fill) + stamp schema.
		if err := s.writeClusterCIDRCache(ctx, encoded, config.CurrentClusterCIDRsSchema, false); err != nil {
			plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: write cache: %v", err)
			return
		}
		plog.G(ctx).Infof("[TunConfig] warmed cluster CIDR cache (raw): %s", encoded)
		return
	}

	// Stale cache (legacy/buggy) + fresh non-empty detection -> overwrite + restamp.
	if err := s.writeClusterCIDRCache(ctx, encoded, config.CurrentClusterCIDRsSchema, true); err != nil {
		plog.G(ctx).Warnf("[TunConfig] CIDR warm-up: overwrite stale cache: %v", err)
		return
	}
	plog.G(ctx).Infof("[TunConfig] re-warmed stale cluster CIDR cache: %s -> %s (schema %s)", cidrVal, encoded, config.CurrentClusterCIDRsSchema)
}

// cacheIsCurrentSchema reports whether a schema value read from the ConfigMap denotes
// the current CIDR-cache schema. Absent/empty/unparseable values read as legacy
// (older than current) so a pre-versioning or buggy cache gets re-validated.
func cacheIsCurrentSchema(schemaVal string) bool {
	return strings.TrimSpace(schemaVal) == config.CurrentClusterCIDRsSchema
}

// encodeCIDRSet serializes CIDRs into a deduplicated space-separated string, matching
// the CLUSTER_CIDRs ConfigMap format read by the client (handler.parseCachedCIDRs).
func encodeCIDRSet(cidrs []*net.IPNet) string {
	set := sets.New[string]()
	for _, c := range cidrs {
		set.Insert(c.String())
	}
	return strings.Join(set.UnsortedList(), " ")
}
