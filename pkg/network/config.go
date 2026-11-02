package network

import (
	"context"
	"net"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/wencaiwulue/kubevpn/v2/pkg/ssh"
)

// Config holds immutable configuration for Runtime.
type Config struct {
	Clientset         kubernetes.Interface
	RESTClient        *rest.RESTClient
	Config            *rest.Config
	ManagerNamespace  string
	WorkloadNamespace string
	CIDRs             []*net.IPNet
	APIServerIPs      []net.IP
	RouteConfig       *RouteConfig
	Image             string
	Lock              *sync.Mutex // shared lock for DNS operations
	OwnerID           string      // unique ID for TunConfigService (UUID)
	Hostname          string      // local machine name, recorded in TUN_ALLOCS for debugging

	// GetRunningPodList returns running traffic manager pods.
	GetRunningPodList func(ctx context.Context) ([]v1.Pod, error)

	// ReservedTunIPs returns TUN IPs already held by sibling connections (other
	// clusters) in the same daemon. They are excluded from this connection's
	// allocation so two clusters don't assign the same local TUN IP. Optional.
	ReservedTunIPs func() []net.IP

	// SshConf, when non-nil, enables automatic SSH data-plane probe: if the SSH
	// host can reach the traffic manager's ClusterIP, port forwarding is done via
	// SSH tunnel instead of kubectl port-forward (bypassing the API server).
	SshConf *ssh.SshConfig
}

type RouteConfig struct {
	ExtraCIDR   []string
	ExtraDomain []string
	ExtraNodeIP bool
}
