package handler

import (
	"context"
	"net"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

// Connection is the shared API surface that EVERY session satisfies — the common
// identity, lifecycle, and status-read methods the daemon layer (pkg/daemon/action)
// uses across both planes. Server.connections is typed []Connection so the daemon
// is decoupled from the concrete structs.
//
// Plane-specific operations do NOT live here. They live on two role interfaces that
// embed Connection — DataPlane (root daemon only) and ProxyController (user daemon
// only). This is Interface Segregation: previously every session carried stub
// implementations of the other plane's methods (e.g. ConnectOptions.DoConnect
// returned an error, DataSession.CreateRemoteInboundPod was a no-op), which let a
// wrong-plane call compile and fail at runtime. Consumers now type-assert to the
// role they need (see resolveTunIP, sort.Connects.Less, disconnect/leave/unsync in
// daemon/action); the assertion's zero-value fallback preserves the old stub
// behavior, so the split is behavior-preserving.
type Connection interface {
	// --- Initialization ---

	// InitClient initializes Kubernetes clients from the given factory.
	InitClient(f cmdutil.Factory) error

	// --- Lifecycle ---

	// Cleanup releases DHCP leases, leaves proxy resources, and runs rollback functions.
	Cleanup(ctx context.Context)

	// Context returns the connection session's context.
	Context() context.Context

	// AddRollbackFunc registers a cleanup function to be called on teardown.
	AddRollbackFunc(f func() error)

	// --- Identity & Status ---

	// GetConnectionID returns the connection identifier (namespace UID suffix) for this session.
	GetConnectionID() string

	// GetOwnerID returns the UUID prefix identifying this connection's envoy rule ownership.
	// Empty string when the connection is not in the user daemon (e.g. root daemon does not use envoy).
	GetOwnerID() string

	// GetWorkloadNamespace returns the user workload namespace for this connection.
	GetWorkloadNamespace() string

	// GetManagerNamespace returns the namespace where the traffic manager is deployed.
	GetManagerNamespace() string

	// GetOriginKubeconfigPath returns the original kubeconfig file path.
	GetOriginKubeconfigPath() string

	// GetFactory returns the kubectl factory.
	GetFactory() cmdutil.Factory

	// GetClientset returns the Kubernetes clientset.
	GetClientset() kubernetes.Interface

	// GetRunningPodList returns the list of pods currently running for the traffic manager.
	GetRunningPodList(ctx context.Context) ([]v1.Pod, error)

	// GetTrafficManagerConfigMap returns the traffic manager ConfigMap from the informer cache.
	GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error)

	// RefreshConfigMapCache does a direct API GET and updates the informer store,
	// so the next cache read sees the latest ConfigMap without waiting for a watch event.
	RefreshConfigMapCache(ctx context.Context) error

	// GetExtraCIDR returns the extra CIDR strings configured for this connection.
	GetExtraCIDR() []string

	// GetSync returns the SyncOptions associated with this connection, or nil when not
	// applicable (the root daemon does not own sync). A safe read for both planes.
	GetSync() *SyncOptions

	// GetSocksListenAddr returns the listen address of the managed SOCKS5 proxy, or "" if none.
	// "" on the root daemon, which does not run a SOCKS proxy.
	GetSocksListenAddr() string

	// GetSocksEgress reports whether the managed SOCKS5 proxy runs in host-direct egress mode.
	// False on the root daemon.
	GetSocksEgress() bool
}

// DataPlane is the root-daemon (data plane) extension of Connection. It exposes
// the operations only a DataSession owns: establishing the TUN/route/DNS
// connection, reading the allocated TUN IP, the data-plane heartbeat, the API
// server IPs, and accumulated extra DNS hosts (all held by NetworkManager in the
// root daemon). Only *DataSession satisfies DataPlane; *ConnectOptions does not.
type DataPlane interface {
	Connection

	// DoConnect establishes the VPN connection (TUN device, routes, DNS).
	DoConnect(ctx context.Context) error

	// GetLocalTunIP returns the local TUN device IPv4 and IPv6 addresses as strings.
	GetLocalTunIP() (v4, v6 string)

	// GetAPIServerIPs returns the Kubernetes API server IPs resolved at connect time.
	GetAPIServerIPs() []net.IP

	// GetNetworkExtraHost returns extra DNS host entries accumulated during route setup.
	GetNetworkExtraHost() []dns.Entry

	// GetLastHeartbeat returns the time of the last observed data-plane heartbeat echo reply.
	// Returns the zero time when no heartbeat has been received or NetworkManager is nil.
	GetLastHeartbeat() time.Time
}

// ProxyController is the user-daemon (control plane) extension of Connection. It
// exposes the sidecar-injection / leave / SOCKS / sync-mutation operations only a
// ConnectOptions owns. Only *ConnectOptions satisfies ProxyController; *DataSession
// does not.
type ProxyController interface {
	Connection

	// CreateRemoteInboundPod injects Envoy sidecar proxies into the specified workloads.
	CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string, localTunIPv4, localTunIPv6 string) error

	// LeaveAllProxyResources removes all proxy sidecar injections for this connection.
	LeaveAllProxyResources(ctx context.Context) error

	// LeaveResource unpatches the given proxy resources.
	LeaveResource(ctx context.Context, resources []Resources, ownerID string) error

	// ProxyResources returns the list of workloads currently being proxied.
	ProxyResources() ProxyList

	// SetSync stores the SyncOptions associated with this connection.
	SetSync(s *SyncOptions)
}

// Compile-time assertions: each session type satisfies the shared Connection
// interface plus exactly the role interface for its plane — and NOT the other
// plane's role (the absence of those assertions is the point: a *ConnectOptions
// cannot be assigned to a DataPlane, and a *DataSession cannot be a ProxyController).
var (
	_ Connection     = (*ConnectOptions)(nil) // ControlSession (user daemon)
	_ ProxyController = (*ConnectOptions)(nil)

	_ Connection = (*DataSession)(nil) // DataSession (root daemon)
	_ DataPlane = (*DataSession)(nil)
)
