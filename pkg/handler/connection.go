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

// Connection defines the API surface that the daemon layer (pkg/daemon/action)
// uses on a connection object. This interface documents the boundary between
// the daemon's session management and the handler's connection implementation.
//
// Both *ConnectOptions (control plane) and *DataSession (data plane) satisfy this
// interface (see the compile-time assertions below); Server.connections is typed
// as []Connection so the daemon is decoupled from the concrete structs.
type Connection interface {
	// --- Initialization ---

	// InitClient initializes Kubernetes clients from the given factory.
	InitClient(f cmdutil.Factory) error

	// --- Lifecycle ---

	// DoConnect establishes the VPN connection (TUN device, routes, DNS).
	DoConnect(ctx context.Context) error

	// Cleanup releases DHCP leases, leaves proxy resources, and runs rollback functions.
	Cleanup(ctx context.Context)

	// Context returns the connection session's context.
	Context() context.Context

	// AddRollbackFunc registers a cleanup function to be called on teardown.
	AddRollbackFunc(f func() error)

	// --- Identity & Status ---

	// GetConnectionID returns the connection identifier (namespace UID suffix) for this session.
	GetConnectionID() string

	// GetLocalTunIP returns the local TUN device IPv4 and IPv6 addresses as strings.
	GetLocalTunIP() (v4, v6 string)

	// --- Proxy Management ---

	// CreateRemoteInboundPod injects Envoy sidecar proxies into the specified workloads.
	CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string, localTunIPv4, localTunIPv6 string) error

	// LeaveAllProxyResources removes all proxy sidecar injections for this connection.
	LeaveAllProxyResources(ctx context.Context) error

	// LeaveResource unpatches the given proxy resources.
	LeaveResource(ctx context.Context, resources []Resources, ownerID string) error

	// ProxyResources returns the list of workloads currently being proxied.
	ProxyResources() ProxyList

	// --- Accessors for fields used by daemon layer ---

	// GetFactory returns the kubectl factory.
	GetFactory() cmdutil.Factory

	// GetClientset returns the Kubernetes clientset.
	GetClientset() kubernetes.Interface

	// GetManagerNamespace returns the namespace where the traffic manager is deployed.
	GetManagerNamespace() string

	// GetOriginKubeconfigPath returns the original kubeconfig file path.
	GetOriginKubeconfigPath() string

	// GetSync returns the SyncOptions associated with this connection, or nil.
	GetSync() *SyncOptions

	// SetSync stores the SyncOptions associated with this connection.
	SetSync(s *SyncOptions)

	// GetRunningPodList returns the list of pods currently running for the traffic manager.
	GetRunningPodList(ctx context.Context) ([]v1.Pod, error)

	// GetLastHeartbeat returns the time of the last observed data-plane heartbeat echo reply.
	// Returns the zero time when no heartbeat has been received or NetworkManager is nil.
	GetLastHeartbeat() time.Time

	// GetTrafficManagerConfigMap returns the traffic manager ConfigMap from the informer cache.
	GetTrafficManagerConfigMap(ctx context.Context) (*v1.ConfigMap, error)

	// RefreshConfigMapCache does a direct API GET and updates the informer store,
	// so the next cache read sees the latest ConfigMap without waiting for a watch event.
	RefreshConfigMapCache(ctx context.Context) error

	// --- Accessors needed by sort.go and daemon/action layer ---

	// GetOwnerID returns the UUID prefix identifying this connection's envoy rule ownership.
	// Empty string when the connection is not in the user daemon (e.g. root daemon does not use envoy).
	GetOwnerID() string

	// GetWorkloadNamespace returns the user workload namespace for this connection.
	GetWorkloadNamespace() string

	// GetAPIServerIPs returns the Kubernetes API server IPs resolved at connect time.
	// Returns nil when called on a user-daemon ConnectOptions (NetworkManager lives in root daemon).
	GetAPIServerIPs() []net.IP

	// GetExtraCIDR returns the extra CIDR strings configured for this connection.
	GetExtraCIDR() []string

	// GetNetworkExtraHost returns extra DNS host entries accumulated during route setup.
	// Returns nil when called on a user-daemon ConnectOptions (NetworkManager lives in root daemon).
	GetNetworkExtraHost() []dns.Entry
}

// Compile-time assertions: both session types must satisfy the Connection interface.
var _ Connection = (*ConnectOptions)(nil) // ControlSession (user daemon)
var _ Connection = (*DataSession)(nil)    // DataSession (root daemon)
