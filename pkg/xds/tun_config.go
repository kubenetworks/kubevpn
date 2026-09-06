package xds

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dhcp"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
)

// annoTunAllocsRejected is the ConfigMap annotation key carrying the breadcrumb for
// a manual TUN_ALLOCS edit that was not applied (client conflict, IP in use, expiry).
const annoTunAllocsRejected = "kubevpn.io/tun-allocs-rejected"

// TunConfigServer implements the TunConfigService gRPC API.
// It manages TUN IP allocations for sidecars and pushes changes via streams.
type TunConfigServer struct {
	rpc.UnimplementedTunConfigServiceServer

	clientset kubernetes.Interface
	namespace string
	dhcp      *dhcp.Manager

	mu       sync.RWMutex
	allocs   map[string]*tunAllocation // ownerID → allocation
	watchers map[string][]chan *rpc.TunIPResponse
	// lastIPs remembers the IP each ownerID most recently held, even after its
	// lease was reaped, so a reconnecting (stable) owner can reclaim the same IP
	// when it is still free. In-memory only; bounded by the real client count
	// because OwnerID is now stable per machine+user.
	lastIPs map[string]lastIPRecord

	// reapedAt records when an ownerID's lease was reaped (and not since re-acquired).
	// The lease reaper uses it to clean up a truly-abandoned owner's envoy rules after
	// abandonmentTTL — long enough that a sleeping client (whose lease lapsed but which
	// will wake and re-allocate) is NOT affected. Cleared when the owner calls GetTunIP.
	reapedAt map[string]time.Time

	// pendingProposal tracks a dry-run IP change proposed to a client but not yet
	// confirmed. It is pure intent — NO IP is rented while pending. The server never
	// commits an IP change until the client confirms (via GetTunIP{ConfirmIP}), so a
	// conflicting proposal is simply dropped (decline/expiry) with nothing to roll
	// back. In-memory only — lost on restart (proposal vanishes; operator re-edits).
	pendingProposal map[string]proposal

	// rejectedAnno is the latest "manual TUN_ALLOCS edit not applied" breadcrumb.
	// It is written into the ConfigMap by saveAllocs (the single CM writer), so it
	// can never be clobbered by a concurrent allocation persist — every saveAllocs
	// re-applies it. Empty means no rejection to surface. Guarded by mu.
	rejectedAnno string

	// reconcileMu serializes ReconcileAllocsFromConfigMap so two overlapping
	// informer-driven reconciles never act on each other's stale snapshot.
	reconcileMu sync.Mutex

	// routes serves WatchNamespaceRoutes: server-side per-namespace pod/service
	// discovery pushed to clients, replacing each client's cluster-wide list-watch.
	routes *routeBroadcaster

	// factory backs server-side sidecar injection (ProxyInject). Set by xds.Main
	// from the in-cluster config; nil in unit tests / when unavailable, in which
	// case ProxyInject returns Unavailable and the client falls back to local
	// injection. Kept separate from clientset because the kubectl injection
	// machinery (GetTopOwnerObject, RolloutStatus, resource.Helper) needs a Factory.
	factory cmdutil.Factory
}

// lastIPRecord is the last IPv4/IPv6 a given owner held.
type lastIPRecord struct {
	v4 *net.IPNet
	v6 *net.IPNet
}

// proposal is a dry-run IP change offered to a client, awaiting confirm/decline.
// candV4/candV6 are per-family candidates (either may be nil = that family is not
// being changed). They hold no bitmap reservation; an IP is rented only at confirm.
type proposal struct {
	candV4   *net.IPNet
	candV6   *net.IPNet
	deadline time.Time
}

type tunAllocation struct {
	IPv4      *net.IPNet
	IPv6      *net.IPNet
	Version   int64
	LastRenew time.Time // last time the client renewed (heartbeat)
	Hostname  string    // client machine name, for debugging which machine owns this lease
}

// persistedAlloc is the YAML-serializable form of tunAllocation.
//
// Tags are json (not yaml): saveAllocs marshals via sigs.k8s.io/yaml, which encodes
// through encoding/json and ignores yaml tags. The keys are therefore honest here —
// they are what actually lands in the TUN_ALLOCS ConfigMap — and omitempty works, so
// a client that sends no hostname leaves a clean entry. Reads are case-insensitive,
// so records written by older builds (capitalized Go-name keys) still load.
type persistedAlloc struct {
	IPv4      string `json:"ipv4"`
	IPv6      string `json:"ipv6"`
	Version   int64  `json:"version"`
	LastRenew int64  `json:"lastRenew"` // unix timestamp
	Hostname  string `json:"hostname,omitempty"`
}

// initDHCPBackoff controls how NewTunConfigServer retries a transient InitDHCP
// failure. A freshly created traffic-manager pod may reach its xds container
// before the pod network / kube-proxy is ready, so the very first ConfigMap read
// can hit "connect: connection refused" to the API server ClusterIP. That window
// closes within seconds, so a bounded backoff (~1 minute total) lets init succeed
// on retry instead of the server coming up permanently without TunConfigService.
// Package-level so tests can shrink it.
var initDHCPBackoff = wait.Backoff{
	Duration: 1 * time.Second,
	Factor:   1.5,
	Steps:    8,
	Cap:      15 * time.Second,
}

// XDSPort is the gRPC port used by the envoy control plane and TunConfigService.
const XDSPort uint = config.PortXDS

// LeaseDuration is how long a TUN IP allocation stays valid without renewal.
// If a client doesn't call GetTunIP (which doubles as renew) within this duration,
// the IP is reclaimed and recycled.
const LeaseDuration = 5 * time.Minute

// leaseReapInterval is how often the lease reaper scans for and reclaims expired allocations.
const leaseReapInterval = 30 * time.Second

// abandonmentTTL is how long after a lease is reaped (with no re-acquire) the owner's
// envoy rules are cleaned up as abandoned. It is deliberately much longer than
// LeaseDuration so a sleeping client — whose lease lapses but which will wake and
// re-allocate, re-pointing its still-present rule via syncEnvoyRuleIP — is not affected;
// only a client gone this long (crash / SIGKILL / powered off) has its rule removed.
// Default 24h; override with KUBEVPN_PROXY_ABANDON_TTL (a Go duration, e.g. "6h").
var abandonmentTTL = resolveAbandonmentTTL()

func resolveAbandonmentTTL() time.Duration {
	if v := os.Getenv("KUBEVPN_PROXY_ABANDON_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 24 * time.Hour
}

// manualGrace is how long a manual TUN_ALLOCS edit keeps the owner's pre-edit IP
// rented (a reservation) after pushing the new IP, awaiting client accept/reject.
// On expiry with no reject the change is assumed accepted and the old IP released.
const manualGrace = 60 * time.Second

// maxReconcileRounds bounds the re-read/merge loop in ReconcileAllocsFromConfigMap
// that absorbs concurrent operator edits landing during a reconcile.
const maxReconcileRounds = 5

// NewTunConfigServer creates and initializes a TunConfigServer.
// Performs DHCP init and loads persisted allocations from ConfigMap.
// Server restart does not affect clients — they keep their same IPs.
//
// InitDHCP is a hard prerequisite for the data plane: without it the caller must
// NOT register TunConfigService (clients would get "unknown service" forever).
// A transient API-connectivity error at fresh-pod startup is therefore retried,
// and only a persistent failure is returned — the caller treats that as fatal so
// the container restarts and retries from a clean state.
func NewTunConfigServer(ctx context.Context, clientset kubernetes.Interface, namespace string) (*TunConfigServer, error) {
	mgr := dhcp.NewDHCPManager(clientset, namespace)
	if err := initDHCPWithRetry(ctx, mgr); err != nil {
		return nil, fmt.Errorf("init DHCP: %w", err)
	}
	s := &TunConfigServer{
		clientset:       clientset,
		namespace:       namespace,
		dhcp:            mgr,
		allocs:          make(map[string]*tunAllocation),
		watchers:        make(map[string][]chan *rpc.TunIPResponse),
		lastIPs:         make(map[string]lastIPRecord),
		reapedAt:        make(map[string]time.Time),
		pendingProposal: make(map[string]proposal),
	}
	s.routes = newRouteBroadcaster(clientset)
	s.loadAllocs(ctx)
	// Reclaim any bitmap bits left orphaned by a prior crash before serving.
	s.scrubOrphanBits(ctx)
	return s, nil
}

// initDHCPWithRetry runs mgr.InitDHCP with a bounded exponential backoff, so a
// transient API-server connectivity blip at fresh-pod startup does not leave the
// xds server permanently without TunConfigService. It returns the last InitDHCP
// error (not the generic backoff-timeout) so the caller can log why it gave up.
func initDHCPWithRetry(ctx context.Context, mgr *dhcp.Manager) error {
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, initDHCPBackoff, func(ctx context.Context) (bool, error) {
		if err := mgr.InitDHCP(ctx); err != nil {
			lastErr = err
			plog.G(ctx).Warnf("[TunConfig] InitDHCP failed, will retry: %v", err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if lastErr != nil {
			return lastErr
		}
		return err
	}
	return nil
}
