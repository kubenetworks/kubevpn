package handler

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// Connection defines the API surface that the daemon layer (pkg/daemon/action)
// uses on a connection object. This interface documents the boundary between
// the daemon's session management and the handler's connection implementation.
//
// For now, only *ConnectOptions satisfies this interface. In a future migration,
// Server.connections can be typed as []Connection to decouple daemon from the
// concrete struct.
type Connection interface {
	// --- Initialization ---

	// InitClient initializes Kubernetes clients from the given factory.
	InitClient(f cmdutil.Factory) error

	// InitDHCP initializes the DHCP manager for IP allocation.
	InitDHCP(ctx context.Context) error

	// RentIP leases IPv4 and IPv6 TUN addresses from DHCP.
	RentIP(ctx context.Context, ipv4, ipv6 string) (context.Context, error)

	// GetIPFromContext extracts IPv4 and IPv6 TUN addresses from incoming gRPC metadata.
	GetIPFromContext(ctx context.Context, logger *log.Logger) error

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

	// GetConnectionID returns the DHCP connection identifier for this session.
	GetConnectionID() string

	// GetLocalTunIP returns the local TUN device IPv4 and IPv6 addresses as strings.
	GetLocalTunIP() (v4, v6 string)

	// GetTunDeviceName returns the OS network interface name of the TUN device.
	GetTunDeviceName() (string, error)

	// IsMe reports whether this connection owns the proxy for the given namespace, UID, and headers.
	IsMe(ns, uid string, headers map[string]string) bool

	// --- Health ---

	// HealthCheckOnce performs a single health check with the given timeout.
	HealthCheckOnce(ctx context.Context, timeout time.Duration)

	// HealthPeriod periodically syncs health status on the given interval.
	HealthPeriod(ctx context.Context, interval time.Duration)

	// HealthStatus returns the last known health state.
	HealthStatus() HealthStatus

	// --- Proxy Management ---

	// CreateRemoteInboundPod injects Envoy sidecar proxies into the specified workloads.
	CreateRemoteInboundPod(ctx context.Context, namespace string, workloads []string, headers map[string]string, portMap []string, image string) error

	// LeaveAllProxyResources removes all proxy sidecar injections for this connection.
	LeaveAllProxyResources(ctx context.Context) error

	// LeaveResource unpatches the given proxy resources.
	LeaveResource(ctx context.Context, resources []Resources, v4 string) error

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
}

// Compile-time assertion: *ConnectOptions must satisfy the Connection interface.
var _ Connection = (*ConnectOptions)(nil)
