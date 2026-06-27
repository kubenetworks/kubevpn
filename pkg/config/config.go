package config

import (
	"net"
	"sync"
	"time"

	"sigs.k8s.io/kustomize/api/konfig"
)

const (
	// ConfigMapPodTrafficManager is the name of the ConfigMap used by the traffic manager.
	ConfigMapPodTrafficManager = "kubevpn-traffic-manager"

	// HelmAppNameKubevpn is the Helm release name for kubevpn.
	HelmAppNameKubevpn = "kubevpn"

	// DefaultNamespaceKubevpn is the default namespace where kubevpn is installed.
	DefaultNamespaceKubevpn = "kubevpn"

	// KeyDHCP is the ConfigMap key for IPv4 DHCP allocation data.
	KeyDHCP = "DHCP"
	// KeyDHCP6 is the ConfigMap key for IPv6 DHCP allocation data.
	KeyDHCP6 = "DHCP6"
	// KeyEnvoy is the ConfigMap key for the Envoy proxy configuration.
	KeyEnvoy = "ENVOY_CONFIG"
	// KeyClusterIPv4POOLS is the ConfigMap key for cluster IPv4 address pools.
	KeyClusterIPv4POOLS = "IPv4_POOLS"

	// TLSCertKey is the key for tls certificates in a TLS secret.
	TLSCertKey = "tls_crt"
	// TLSPrivateKeyKey is the key for the private key field in a TLS secret.
	TLSPrivateKeyKey = "tls_key"
	// TLSServerName for tls config server name
	TLSServerName = "tls_server_name"

	// ContainerSidecarEnvoyProxy is the container name for the Envoy proxy sidecar.
	ContainerSidecarEnvoyProxy = "envoy-proxy"
	// ContainerSidecarControlPlane is the container name for the xDS control plane sidecar.
	ContainerSidecarControlPlane = "control-plane"
	// ContainerSidecarWebhook is the container name for the admission webhook sidecar.
	ContainerSidecarWebhook = "webhook"
	// ContainerSidecarVPN is the container name for the VPN sidecar.
	ContainerSidecarVPN = "vpn"
	// ContainerSidecarSyncthing is the container name for the Syncthing file-sync sidecar.
	ContainerSidecarSyncthing = "syncthing"

	// VolumeSyncthing is the volume name for the Syncthing shared directory.
	VolumeSyncthing = "syncthing"

	// PortNameTCP is the traffic-manager service port name for TCP tunneling.
	PortNameTCP = "10801-for-tcp"
	// PortNameEnvoy is the traffic-manager service port name for the Envoy control plane.
	PortNameEnvoy = "9002-for-envoy"
	// PortNameHTTP is the traffic-manager service port name for the admission webhook.
	PortNameHTTP = "80-for-webhook"
	// PortNameDNS is the traffic-manager service port name for DNS resolution.
	PortNameDNS = "53-for-dns"

	// IPv4Pool is used as TUN IP
	// 198.18.0.0/16 network is part of the 198.18.0.0/15 (reserved for benchmarking).
	// https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml
	// so we split it into 2 parts: 198.18.0.0/15 --> [198.18.0.0/16, 198.19.0.0/16]
	IPv4Pool = "198.18.0.0/16"
	// IPv6Pool is the IPv6 CIDR used for TUN device address allocation.
	// 2001:2::/64 network is part of the 2001:2::/48 (reserved for benchmarking)
	// https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
	IPv6Pool = "2001:2::/64"
	// DockerIPv4Pool is the IPv4 CIDR used for kubevpn Docker bridge networks.
	/*
		reason：docker use 172.17.0.0/16 network conflict with k8s service kubernetes
		➜  ~ kubectl get service kubernetes
		NAME         TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
		kubernetes   ClusterIP   172.17.0.1   <none>        443/TCP   190d

		➜  ~ docker network inspect bridge | jq '.[0].IPAM.Config'
		[
		 {
		   "Subnet": "172.17.0.0/16",
		   "Gateway": "172.17.0.1"
		 }
		]
	*/
	DockerIPv4Pool = "198.19.0.1/16"

	// DefaultNetDir is the default directory for CNI network configuration files.
	DefaultNetDir = "/etc/cni/net.d"

	// Proc is the path to the Linux procfs mount.
	Proc = "/proc"

	// CniNetName is the volume name for the CNI network config directory.
	CniNetName = "cni-net-dir-kubevpn"

	// EnvInboundPodTunIPv4 is the env var name for the pod's TUN device IPv4 address.
	EnvInboundPodTunIPv4 = "TunIPv4"
	// EnvInboundPodTunIPv6 is the env var name for the pod's TUN device IPv6 address.
	EnvInboundPodTunIPv6 = "TunIPv6"
	// EnvPodName is the env var name populated with the Kubernetes pod name.
	EnvPodName = "POD_NAME"
	// EnvPodNamespace is the env var name populated with the Kubernetes pod namespace.
	EnvPodNamespace = "POD_NAMESPACE"

	// HeaderIPv4 is the HTTP header name carrying the client's TUN IPv4 address.
	HeaderIPv4 = "IPv4"
	// HeaderIPv6 is the HTTP header name carrying the client's TUN IPv6 address.
	HeaderIPv6 = "IPv6"

	// KUBECONFIG is the key name used when storing kubeconfig data.
	KUBECONFIG = "kubeconfig"

	// ManageBy is the label key indicating the resource is managed by kubevpn.
	ManageBy = konfig.ManagedbyLabelKey

	// PProfPort is the pprof HTTP server port for the user daemon.
	PProfPort = 32345
	// SudoPProfPort is the pprof HTTP server port for the root daemon.
	SudoPProfPort = 33345
	// PProfDir is the directory name for storing pprof profiles.
	PProfDir = "pprof"

	// EnvSSHJump is the env var set when connected via an SSH jump host.
	EnvSSHJump = "SSH_JUMP_BY_KUBEVPN"

	// HostsKeyword is the marker comment appended to /etc/hosts entries managed by kubevpn.
	HostsKeyword = "Added by KubeVPN"
	// HostsDeviceKeyword is the per-device marker comment template for /etc/hosts entries.
	HostsDeviceKeyword = "# For dev %s " + HostsKeyword
)

var (
	// Image is the container image used for injected sidecars, set via -ldflags.
	Image = "ghcr.io/kubenetworks/kubevpn:latest"
	// Version is the build version string, set via -ldflags.
	Version = "latest"
	// GitCommit is the git commit SHA at build time, set via -ldflags.
	GitCommit = ""

	// GitHubOAuthToken is the GitHub OAuth token for release checks, set via -ldflags.
	GitHubOAuthToken = ""

	// OriginImage is the unmodified container image tag derived from Version.
	OriginImage = "ghcr.io/kubenetworks/kubevpn:" + Version
)

var (
	// CIDR is the parsed IPv4 network used for TUN address allocation.
	CIDR *net.IPNet
	// CIDR6 is the parsed IPv6 network used for TUN address allocation.
	CIDR6 *net.IPNet
	// RouterIP is the gateway IPv4 address within CIDR.
	RouterIP net.IP
	// RouterIP6 is the gateway IPv6 address within CIDR6.
	RouterIP6 net.IP

	// DockerCIDR is the IPv4 network used for the kubevpn Docker bridge.
	DockerCIDR *net.IPNet
	// DockerRouterIP is the gateway IPv4 address within DockerCIDR.
	DockerRouterIP net.IP
)

func init() {
	var err error
	RouterIP, CIDR, err = net.ParseCIDR(IPv4Pool)
	if err != nil {
		panic(err)
	}
	RouterIP6, CIDR6, err = net.ParseCIDR(IPv6Pool)
	if err != nil {
		panic(err)
	}
	DockerRouterIP, DockerCIDR, err = net.ParseCIDR(DockerIPv4Pool)
	if err != nil {
		panic(err)
	}
}

// Debug enables verbose debug logging when true.
var Debug bool

var (
	// LargeBufferSize is the byte size for large I/O buffers (64KB).
	LargeBufferSize = 64 * 1024
)

var (
	KeepAliveTime  = 60 * time.Second
	DialTimeout    = 15 * time.Second
	ConnectTimeout = 5 * time.Second

	DNSRouteRefreshInterval  = 15 * time.Second
	DNSRouteDebounceInterval = 3 * time.Second

	UDPSessionTimeout = 120 * time.Second
	UDPRelayTimeout   = 30 * time.Second

	SlotReconnectBackoff = 2 * time.Second
	DaemonPollInterval   = 200 * time.Millisecond
)

var (
	// DefaultMTU is the TUN device MTU, calculated to fit within a 1500-byte Ethernet frame after IP/TCP/TLS overhead.
	/**
	  +--------------------------------------------------------------------+
	  |                     Original IP Packet from TUN                    |
	  +-------------------+------------------------------------------------+
	  |  IP Header (20B)  |               Payload (MTU size)               |
	  +-------------------+------------------------------------------------+


	  After adding custom 2-byte header:
	  +----+-------------------+-------------------------------------------+
	  | LH |  IP Header (20B)  |               Payload                     |
	  +----+-------------------+-------------------------------------------+
	  | 2B |        20B        |          1453 - 20 = 1433B                |
	  +----+-------------------+-------------------------------------------+

	  TLS 1.3 Record Structure Breakdown:
	  +---------------------+--------------------------+-------------------+
	  | TLS Header (5B)     | Encrypted Data (N)       | Auth Tag (16B)    |
	  +---------------------+--------------------------+-------------------+
	  |  Content Type (1)   |          ↑               | AEAD Authentication
	  |   Version (2)       |   Encrypted Payload      | (e.g. AES-GCM)    |
	  |   Length (2)        |  (Original Data + LH2)   |                   |
	  +---------------------+--------------------------+-------------------+
	  |←------- 5B --------→|←---- Length Field ------→|←----- 16B -------→|


	  Final Ethernet Frame:
	  +--------+----------------+----------------+-----------------------+--------+
	  | EthHdr | IP Header      | TCP Header     | TLS Components                 |
	  | (14B)  | (20B)          | (20B)          +---------+-------------+--------+
	  |        |                |                | Hdr(5B) | Data+LH2    | Tag(16)|
	  +--------+----------------+----------------+---------+-------------+--------+
	  |←------------------- Total 1500B Ethernet Frame --------------------------→|

	  ipv4: 20
	  ipv6: 40

	mtu = 1500 - ip header(20/40 v4/v6) - tcp header (20) - tls1.3(5+1+16) - packet over tcp(length(2)+remark(1)) = 1415
	*/
	DefaultMTU = 1500 - max(20, 40) - 20 - (5 + 1 + 16) - (2 + 1)
)

var (
	// LPool is a sync.Pool of LargeBufferSize byte slices for reuse in I/O paths.
	LPool = sync.Pool{
		New: func() any {
			return make([]byte, LargeBufferSize)
		},
	}
)

// Slogan is the success message printed after a connection is established.
const Slogan = "Now you can access resources in the kubernetes cluster !"
