package handler

import (
	"context"
	"net"
	"time"

	v1 "k8s.io/api/core/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"

	"github.com/wencaiwulue/kubevpn/v2/pkg/dns"
)

// Connection contains shared identity, lifecycle and value-only status reads.
// Kubernetes clients, initialization, proxy and sync resources belong to their
// concrete sessions or role interfaces, not the daemon's membership registry.
type Connection interface {
	Cleanup(context.Context)
	Context() context.Context
	GetConnectionID() string
	GetOwnerID() string
	GetWorkloadNamespace() string
	GetManagerNamespace() string
	GetOriginKubeconfigPath() string
	GetClusterName() string
	GetExtraCIDR() []string
	StatusSnapshot(context.Context) (SessionStatus, error)
}

// SessionStatus is a read-only copy for status rendering, not a live cache/client.
type SessionStatus struct {
	Envoy string
	Sync  *SyncStatus
}

type SyncStatus struct {
	Workloads           []string
	Headers             map[string]string
	TargetWorkloadNames map[string]string
	SyncthingGUIAddr    string
}

// DataPlane is the root-daemon role. Only DataSession establishes networking.
type DataPlane interface {
	Connection
	InitClient(cmdutil.Factory) error
	AddRollbackFunc(func() error)
	DoConnect(context.Context) error
	GetLocalTunIP() (string, string)
	GetAPIServerIPs() []net.IP
	GetNetworkExtraHost() []dns.Entry
	GetLastHeartbeat() time.Time
}

// ProxyController owns control-plane proxy and sync operations. Initialization
// and kubeconfig recovery use the concrete control session, not shared Connection.
type ProxyController interface {
	Connection
	CreateRemoteInboundPod(context.Context, string, []string, map[string]string, []string, string, string, string) error
	LeaveAllProxyResources(context.Context) error
	LeaveResource(context.Context, []Resources, string) error
	ProxyResources() ProxyList
	GetRunningPodList(context.Context) ([]v1.Pod, error)
	RefreshConfigMapCache(context.Context) error
	GetSocksListenAddr() string
	GetSocksEgress() bool
	GetSync() *SyncOptions
	SetSync(*SyncOptions)
}

var (
	_ Connection      = (*ConnectOptions)(nil)
	_ ProxyController = (*ConnectOptions)(nil)
	_ Connection      = (*DataSession)(nil)
	_ DataPlane       = (*DataSession)(nil)
)
