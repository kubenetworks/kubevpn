package handler

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// ConnectOptions holds all state for a user-daemon (control-plane) kubevpn connection session.
// It is the type stored in svr.connections when IsSudo=false.
//
// Type alias ControlSession = ConnectOptions is provided for semantic clarity.
// All existing code that uses ConnectOptions continues to compile unchanged.
type ConnectOptions struct {
	SessionBase

	// Persisted for daemon restart replay (json tag required).
	RequestRaw []byte `json:"RequestRaw,omitempty"`

	// Identity: set from the incoming ConnectRequest and persisted (json tags).
	ManagerNamespace     string
	WorkloadNamespace    string
	ExtraRouteInfo       ExtraRouteInfo
	OriginKubeconfigPath string
	Image                string
	ImagePullSecretName  string
	OwnerID              string `json:"OwnerID,omitempty"`
	ConnectionID         string

	// Control-plane only: SSH jump host IPs (added to API server IPs at CIDR detection).
	SshHosts []net.IP `json:"-"`

	// Control-plane only: sidecar injection lifecycle.
	proxyManager *ProxyManager

	// Control-plane only: file sync.
	syncMu sync.RWMutex
	Sync   *SyncOptions

	// Control-plane only: user-daemon-managed local SOCKS5 proxy for this connection.
	// Persisted so status can report them and daemon-restart replay reflects them (the proxy
	// goroutine itself is stopped via a rollback func and restarted from the replayed
	// ConnectRequest, so no live handle is stored here).
	SocksListenAddr string `json:"SocksListenAddr,omitempty"`
	SocksEgress     bool   `json:"SocksEgress,omitempty"`
}

// GetSocksListenAddr returns the listen address of the managed SOCKS5 proxy, or "" if none.
func (c *ConnectOptions) GetSocksListenAddr() string { return c.SocksListenAddr }

// GetSocksEgress reports whether the managed SOCKS5 proxy runs in host-direct egress mode.
func (c *ConnectOptions) GetSocksEgress() bool { return c.SocksEgress }

// InitClient initializes the Kubernetes clientset, REST client, and config from the given factory.
func (c *ConnectOptions) InitClient(f cmdutil.Factory) error {
	var err error
	c.ManagerNamespace, err = c.K8sClient.InitClient(f)
	return err
}

// Context is a stub on the control-plane session (ConnectOptions/ControlSession).
// The data-plane context lives in DataSession; this returns nil for all user-daemon instances.
func (c *ConnectOptions) Context() context.Context {
	return nil
}

// GetConnectionID returns the connection identifier (namespace UID suffix) for this session.
func (c *ConnectOptions) GetConnectionID() string {
	if c == nil {
		return ""
	}
	return c.ConnectionID
}

// GetManagerNamespace returns the namespace where the traffic manager is deployed.
func (c *ConnectOptions) GetManagerNamespace() string {
	return c.ManagerNamespace
}

// GetWorkloadNamespace returns the user workload namespace for this connection.
func (c *ConnectOptions) GetWorkloadNamespace() string {
	return c.WorkloadNamespace
}

// GetOwnerID returns the OwnerID field.
func (c *ConnectOptions) GetOwnerID() string {
	if c == nil {
		return ""
	}
	return c.OwnerID
}

// GetOriginKubeconfigPath returns the original kubeconfig file path.
func (c *ConnectOptions) GetOriginKubeconfigPath() string {
	return c.OriginKubeconfigPath
}

// GetExtraCIDR returns the extra CIDR strings configured for this connection.
func (c *ConnectOptions) GetExtraCIDR() []string {
	return c.ExtraRouteInfo.ExtraCIDR
}

// GetRunningPodList returns the running traffic manager pods in the manager namespace.
func (c *ConnectOptions) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
	return c.SessionBase.GetRunningPodList(ctx, c.ManagerNamespace)
}

// getConfigMapStore returns the ConfigMapStore, creating it lazily on first access.
// This must be lazy because ManagerNamespace may be updated by detectAndSetManagerNamespace
// after InitClient returns (user daemon path).
func (c *ConnectOptions) getConfigMapStore() *ConfigMapStore {
	return c.SessionBase.getConfigMapStore(c.ManagerNamespace)
}

// Set updates a key-value pair in the traffic manager ConfigMap.
func (c *ConnectOptions) Set(ctx context.Context, key, value string) error {
	return c.getConfigMapStore().Set(ctx, key, value)
}

// Get retrieves a value by key from the traffic manager ConfigMap via the informer cache.
func (c *ConnectOptions) Get(ctx context.Context, key string) (string, error) {
	return c.getConfigMapStore().Get(ctx, key)
}

// EnsureConfigMapSynced warms the traffic manager ConfigMap informer at connection
// establishment so later reads are cache-served (the store has no live-API fallback).
func (c *ConnectOptions) EnsureConfigMapSynced(ctx context.Context) error {
	return c.getConfigMapStore().EnsureSynced(ctx)
}

// GetTrafficManagerConfigMap returns the traffic manager ConfigMap from the warm informer
// cache (see EnsureConfigMapSynced). Returns (nil, nil) on a cache miss — never a blocking
// live-API GET.
func (c *ConnectOptions) GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error) {
	return c.getConfigMapStore().GetConfigMap(ctx)
}

func (c *ConnectOptions) RefreshConfigMapCache(ctx context.Context) error {
	return c.getConfigMapStore().Refresh(ctx)
}

// GetConfigMapInformer returns a shared informer for the traffic manager ConfigMap.
// Created once on first call, then reused. Thread-safe via sync.Once.
// Must be called after InitClient.
func (c *ConnectOptions) GetConfigMapInformer() cache.SharedInformer {
	return c.getConfigMapStore().GetInformer()
}

// ProxyResources returns the list of workloads currently being proxied by this connection.
func (c *ConnectOptions) ProxyResources() ProxyList {
	if c.proxyManager == nil {
		return nil
	}
	return c.proxyManager.Resources()
}

// GetSync returns the SyncOptions associated with this connection, or nil.
func (c *ConnectOptions) GetSync() *SyncOptions {
	c.syncMu.RLock()
	defer c.syncMu.RUnlock()
	return c.Sync
}

// SetSync stores the SyncOptions associated with this connection.
func (c *ConnectOptions) SetSync(s *SyncOptions) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	c.Sync = s
}

// CreateRemoteInboundPod injects Envoy sidecar proxies into the specified workloads for
// inbound traffic interception. Injection runs entirely server-side (the traffic manager
// patches workloads and writes envoy rules with its own ServiceAccount over the VPN); the
// client only starts the local port Mapper for K8s Service workloads. There is no local
// fallback — a manager/RBAC failure is returned to the caller.
func (c *ConnectOptions) CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string, localTunIPv4, localTunIPv6 string) error {
	if localTunIPv4 == "" {
		return fmt.Errorf("local tun IPv4 is empty")
	}
	if err := c.createRemoteInboundViaManager(ctx, namespace, headers, portMap, image, localTunIPv4, localTunIPv6, workloads); err != nil {
		return fmt.Errorf("%w: %w", err, config.ErrEnvoyInject)
	}
	// The server-side inject just wrote envoy rules to the ConfigMap. Refresh the
	// informer cache so the next status query sees ProxyList immediately, without
	// waiting for the asynchronous watch event to propagate.
	_ = c.getConfigMapStore().Refresh(ctx)
	return nil
}

// CreateOutboundPod ensures the traffic manager pod exists and is ready.
// This is a control-plane responsibility and should be called from the user daemon
// before calling the sudo daemon, so the TunConfigService is available for IP allocation.
func (c *ConnectOptions) CreateOutboundPod(ctx context.Context) error {
	if err := createOutboundPod(ctx, c.clientset, c.ManagerNamespace, c.Image, c.ImagePullSecretName); err != nil {
		// Tag generic deploy failures; an already-classified cause (RBAC, pod-ready
		// timeout, image pull) keeps its own, more specific code.
		return fmt.Errorf("%w: %w", err, config.ErrTrafficManagerDeploy)
	}
	// Grant the manager SA list/watch on pods/services in the workload namespace so its
	// server-side route discovery (WatchNamespaceRoutes) works. Runs every connect
	// (idempotent), covering an already-installed / just-upgraded manager. Best-effort:
	// if the user cannot create this RBAC, discovery degrades to CIDR-only routing.
	ensureRouteRBAC(ctx, c.clientset, c.WorkloadNamespace, c.ManagerNamespace)
	// Grant the manager SA the verbs to inject sidecars in the workload namespace, so
	// server-side ProxyInject works. Idempotent, best-effort: if the user cannot create
	// this RBAC, ProxyInject fails and the client falls back to local injection.
	ensureProxyRBAC(ctx, c.clientset, c.WorkloadNamespace, c.ManagerNamespace)
	return nil
}

func dedupAndFilterCIDRs(cidrs []*net.IPNet, apiServerIPs []net.IP) []*net.IPNet {
	return util.RemoveCIDRsContainingIPs(util.RemoveLargerOverlappingCIDRs(cidrs), apiServerIPs)
}

// parseCachedCIDRs parses a space-separated CIDR string (from ConfigMap cache)
// and returns the parsed CIDRs after dedup and API server IP filtering.
func parseCachedCIDRs(ipPoolStr string, apiServerIPs []net.IP) []*net.IPNet {
	var cidrs []*net.IPNet
	for _, s := range strings.Split(ipPoolStr, " ") {
		_, cidr, _ := net.ParseCIDR(s)
		if cidr != nil {
			cidrs = append(cidrs, cidr)
		}
	}
	return dedupAndFilterCIDRs(cidrs, apiServerIPs)
}

// encodeCIDRs serializes CIDRs into a deduplicated space-separated string for ConfigMap storage.
func encodeCIDRs(cidrs []*net.IPNet) string {
	s := sets.New[string]()
	for _, cidr := range cidrs {
		s.Insert(cidr.String())
	}
	return strings.Join(s.UnsortedList(), " ")
}
