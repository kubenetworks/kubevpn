package handler

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/config"
	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
	plog "github.com/wencaiwulue/kubevpn/v2/pkg/log"
	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

// DataSession is the root-daemon (data-plane) session type.
// It is stored in svr.connections when IsSudo=true.
// It is NEVER persisted — OffloadToConfig only type-asserts to *ConnectOptions.
//
// Construction: daemon/action/connect.go (IsSudo=true path).
// DoConnect is the ONLY entry point that starts the network stack.
type DataSession struct {
	SessionBase

	// Identity: set from the incoming ConnectRequest.
	ManagerNamespace     string
	WorkloadNamespace    string
	ExtraRouteInfo       ExtraRouteInfo
	OriginKubeconfigPath string
	Image                string
	ImagePullSecretName  string
	OwnerID              string
	ConnectionID         string

	// Data-plane only: set by root daemon before DoConnect.
	// Lock is from &svr.Lock (DNS shared lock), not from the request proto.
	Lock           *sync.Mutex
	ReservedTunIPs func() []net.IP
	SshConf        *ssh.SshConfig

	// Data-plane lifecycle: set at the START of DoConnect.
	// nil before DoConnect is called (should never occur in practice —
	// DataSession is never stored without DoConnect having been called).
	ctx    context.Context
	cancel context.CancelFunc
	nm     *NetworkManager // the running network stack
}

// InitClient initializes the Kubernetes clientset, REST client, and config from the given factory.
func (ds *DataSession) InitClient(f cmdutil.Factory) error {
	var err error
	ds.ManagerNamespace, err = ds.K8sClient.InitClient(f)
	return err
}

// --- Connection interface: real data-plane implementations ---

// DoConnect establishes the full VPN connection: CIDR detection, port forwarding, TUN device, routing, and DNS.
// ds.ctx/ds.cancel are set at the START so that if DoConnect fails partway, DataSession.Cleanup
// still routes to cleanupDataPlane (which guards ds.nm != nil).
func (ds *DataSession) DoConnect(ctx context.Context) (err error) {
	ds.ctx, ds.cancel = context.WithCancel(ctx)
	plog.G(ctx).Debug("Starting connect to cluster")
	go ds.setupSignalHandler(ds.ctx)

	// Warm the ConfigMap informer before any read (getCIDR reads KeyClusterCIDRs below).
	// The store has no live-API fallback, so a connection whose cache never warms would read
	// empty forever; fail fast instead of silently degrading. CreateOutboundPod already ran in
	// the user daemon, so the ConfigMap exists and the informer can List it — a sync failure
	// here means the API is unreachable, which the subsequent connect steps would fail on anyway.
	if err = ds.getConfigMapStore().EnsureSynced(ds.ctx); err != nil {
		plog.G(ctx).Errorf("Failed to warm traffic manager ConfigMap cache: %v", err)
		err = fmt.Errorf("failed to warm traffic manager ConfigMap cache: %w", err)
		return
	}

	var cidrs []*net.IPNet
	var apiServerIPs []net.IP
	if cidrs, apiServerIPs, err = ds.getCIDR(ds.ctx); err != nil {
		plog.G(ctx).Errorf("Failed to get network CIDR: %v", err)
		return
	}

	hostname, _ := os.Hostname() // best-effort; empty on failure is fine (omitted from TUN_ALLOCS)
	ds.nm = newNetworkManager(NetworkConfig{
		Clientset:         ds.clientset,
		RESTClient:        ds.restclient,
		Config:            ds.config,
		ManagerNamespace:  ds.ManagerNamespace,
		WorkloadNamespace: ds.WorkloadNamespace,
		CIDRs:             cidrs,
		APIServerIPs:      apiServerIPs,
		ExtraRouteInfo:    &ds.ExtraRouteInfo,
		Image:             ds.Image,
		Lock:              ds.Lock,
		OwnerID:           ds.OwnerID,
		Hostname:          hostname,
		GetRunningPodList: ds.GetRunningPodList,
		ReservedTunIPs:    ds.ReservedTunIPs,
		SshConf:           ds.SshConf,
	})
	if err = ds.nm.Start(ds.ctx); err != nil {
		return
	}
	ds.nm.StartIPWatcher(ds.ctx)
	return
}

// Cleanup releases DHCP leases, stops the network stack, and runs rollback functions.
// DataSession is always the data-plane (root daemon) session type; Cleanup always
// runs cleanupDataPlane. Uses the shared SessionBase.cleanup for mutex gating.
func (ds *DataSession) Cleanup(logCtx context.Context) {
	if ds == nil {
		return
	}
	ds.SessionBase.cleanup(logCtx, func(ctx context.Context) error {
		ds.cleanupDataPlane(logCtx)
		return nil
	})
}

// Context returns the connection session's context.
func (ds *DataSession) Context() context.Context {
	return ds.ctx
}

// GetLocalTunIP returns the local TUN device IPv4 and IPv6 addresses as strings.
func (ds *DataSession) GetLocalTunIP() (v4 string, v6 string) {
	if ds.nm == nil {
		return "", ""
	}
	if ip := ds.nm.LocalTunIPv4(); ip != nil {
		v4 = ip.IP.String()
	}
	if ip := ds.nm.LocalTunIPv6(); ip != nil {
		v6 = ip.IP.String()
	}
	return
}

// GetLastHeartbeat returns the time of the last observed data-plane heartbeat echo reply.
func (ds *DataSession) GetLastHeartbeat() time.Time {
	if ds.nm == nil {
		return time.Time{}
	}
	return ds.nm.LastHeartbeat()
}

// GetAPIServerIPs returns the Kubernetes API server IPs resolved at connect time.
func (ds *DataSession) GetAPIServerIPs() []net.IP {
	if ds.nm == nil {
		return nil
	}
	return ds.nm.cfg.APIServerIPs
}

// GetNetworkExtraHost returns extra DNS host entries accumulated during route setup.
func (ds *DataSession) GetNetworkExtraHost() []dns.Entry {
	if ds.nm == nil {
		return nil
	}
	return ds.nm.GetExtraHost()
}

// GetConnectionID returns the connection identifier (namespace UID suffix) for this session.
func (ds *DataSession) GetConnectionID() string {
	if ds == nil {
		return ""
	}
	return ds.ConnectionID
}

// GetManagerNamespace returns the namespace where the traffic manager is deployed.
func (ds *DataSession) GetManagerNamespace() string {
	return ds.ManagerNamespace
}

// GetWorkloadNamespace returns the user workload namespace for this connection.
func (ds *DataSession) GetWorkloadNamespace() string {
	return ds.WorkloadNamespace
}

// GetOwnerID returns the OwnerID field.
func (ds *DataSession) GetOwnerID() string {
	if ds == nil {
		return ""
	}
	return ds.OwnerID
}

// GetOriginKubeconfigPath returns the original kubeconfig file path.
func (ds *DataSession) GetOriginKubeconfigPath() string {
	return ds.OriginKubeconfigPath
}

// GetSocksListenAddr is a data-plane stub: the managed SOCKS5 proxy is a control-plane concern.
func (ds *DataSession) GetSocksListenAddr() string { return "" }

// GetSocksEgress is a data-plane stub: the managed SOCKS5 proxy is a control-plane concern.
func (ds *DataSession) GetSocksEgress() bool { return false }

// GetExtraCIDR returns the extra CIDR strings configured for this connection.
func (ds *DataSession) GetExtraCIDR() []string {
	return ds.ExtraRouteInfo.ExtraCIDR
}

// GetRunningPodList returns the running traffic manager pods in the manager namespace.
func (ds *DataSession) GetRunningPodList(ctx context.Context) ([]v1.Pod, error) {
	return ds.SessionBase.GetRunningPodList(ctx, ds.ManagerNamespace)
}

// getConfigMapStore returns the ConfigMapStore, creating it lazily on first access.
func (ds *DataSession) getConfigMapStore() *ConfigMapStore {
	return ds.SessionBase.getConfigMapStore(ds.ManagerNamespace)
}

// Set updates a key-value pair in the traffic manager ConfigMap.
func (ds *DataSession) Set(ctx context.Context, key, value string) error {
	return ds.getConfigMapStore().Set(ctx, key, value)
}

// Get retrieves a value by key from the traffic manager ConfigMap, using the informer cache first.
func (ds *DataSession) Get(ctx context.Context, key string) (string, error) {
	return ds.getConfigMapStore().Get(ctx, key)
}

// GetTrafficManagerConfigMap returns the traffic manager ConfigMap from the informer cache.
func (ds *DataSession) GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error) {
	return ds.getConfigMapStore().GetConfigMap(ctx)
}

func (ds *DataSession) RefreshConfigMapCache(ctx context.Context) error {
	return ds.getConfigMapStore().Refresh(ctx)
}

// --- Connection interface: control-plane stubs (data-plane session does not proxy workloads) ---

// GetSync returns nil on the data-plane session.
// File sync is a control-plane responsibility; this is a safe read (the method is
// on the shared Connection interface) returning "not applicable", not a stub.
func (ds *DataSession) GetSync() *SyncOptions {
	return nil
}

// --- Internal data-plane helpers ---

// cleanupDataPlane tears down the data-plane side: runs rollback functions,
// stops the networking stack, and cancels the connection context.
func (ds *DataSession) cleanupDataPlane(logCtx context.Context) {
	executeRollbackFuncs(logCtx, ds.getRollbackFuncs())
	if ds.nm != nil {
		plog.G(logCtx).Debugf("Stopping network manager")
		ds.nm.Stop()
	}
	if ds.cancel != nil {
		ds.cancel()
	}
}

// setupSignalHandler waits for a termination signal or context cancellation.
// ctx is passed explicitly from DoConnect rather than read from ds.ctx:
// this goroutine runs concurrently with DoConnect, so taking the context as a
// parameter avoids an unsynchronized read of the ds.ctx field.
func (ds *DataSession) setupSignalHandler(ctx context.Context) {
	stopChan := make(chan os.Signal, 1)
	// SIGKILL and SIGSTOP cannot be caught by signal.Notify, so they are intentionally omitted.
	signal.Notify(stopChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	select {
	case <-stopChan:
		ds.Cleanup(context.Background())
	case <-ctx.Done():
	}
}

// getAPIServerIPs resolves the API server IPs from the kubeconfig host.
// DataSession does not have SSH jump hosts (those are a control-plane concern).
func (ds *DataSession) getAPIServerIPs() ([]net.IP, error) {
	return util.GetAPIServerIP(ds.config.Host)
}

// getCIDR detects cluster Pod/Service CIDRs and API server IPs.
func (ds *DataSession) getCIDR(ctx context.Context) ([]*net.IPNet, []net.IP, error) {
	plog.G(ctx).Debug("Detecting cluster CIDRs")
	apiServerIPs, err := ds.getAPIServerIPs()
	if err != nil {
		return nil, nil, err
	}

	ipPoolStr, err := ds.Get(ctx, config.KeyClusterCIDRs)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(ipPoolStr) != "" {
		cidrs := parseCachedCIDRs(ipPoolStr, apiServerIPs)
		plog.StepDone(ctx, "Detected cluster CIDRs: %s (cached)", util.CIDRsToString(cidrs))
		return cidrs, apiServerIPs, nil
	}

	raw := util.GetCIDR(ctx, ds.clientset, ds.ManagerNamespace)
	cidrs := dedupAndFilterCIDRs(raw, apiServerIPs)
	// Cache the RAW (deduped, unfiltered) CIDRs — NOT the per-client-filtered set.
	// API-server IPs differ per client and every reader re-filters on read via
	// parseCachedCIDRs, so caching a filtered set would hide a CIDR (the one holding
	// this client's API-server IP) from other clients. See docs/46.
	rawDeduped := util.RemoveLargerOverlappingCIDRs(raw)
	if len(rawDeduped) == 0 {
		plog.G(ctx).Debugf("No cluster CIDRs detected (raw=%d)", len(raw))
		return cidrs, apiServerIPs, nil
	}
	encoded := encodeCIDRs(rawDeduped)
	plog.G(ctx).Debugf("Saving %d raw cluster CIDRs to cache: %s", len(rawDeduped), encoded)
	err = ds.Set(ctx, config.KeyClusterCIDRs, encoded)
	return cidrs, apiServerIPs, err
}
