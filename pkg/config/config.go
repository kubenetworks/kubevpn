package config

import (
	"net"
	"sync"
	"time"

	"sigs.k8s.io/kustomize/api/konfig"
)

const (
	// configmap name
	ConfigMapPodTrafficManager = "kubevpn-traffic-manager"

	// helm app name kubevpn
	HelmAppNameKubevpn = "kubevpn"

	// default installed namespace
	DefaultNamespaceKubevpn = "kubevpn"

	// config map keys
	KeyDHCP             = "DHCP"
	KeyDHCP6            = "DHCP6"
	KeyEnvoy            = "ENVOY_CONFIG"
	KeyClusterIPv4POOLS = "IPv4_POOLS"

	// secret keys
	// TLSCertKey is the key for tls certificates in a TLS secret.
	TLSCertKey = "tls_crt"
	// TLSPrivateKeyKey is the key for the private key field in a TLS secret.
	TLSPrivateKeyKey = "tls_key"
	// TLSServerName for tls config server name
	TLSServerName = "tls_server_name"

	// container name
	ContainerSidecarEnvoyProxy   = "envoy-proxy"
	ContainerSidecarControlPlane = "control-plane"
	ContainerSidecarWebhook      = "webhook"
	ContainerSidecarVPN          = "vpn"
	ContainerSidecarSyncthing    = "syncthing"

	VolumeSyncthing = "syncthing"

	// IPv4Pool is used as tun ip
	// 198.19.0.0/16 network is part of the 198.18.0.0/15 (reserved for benchmarking).
	// https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml
	// so we split it into 2 parts: 198.18.0.0/15 --> [198.19.0.0/16, 198.19.0.0/16]
	IPv4Pool = "198.19.0.0/16"
	// 2001:2::/64 network is part of the 2001:2::/48 (reserved for benchmarking)
	// https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
	IPv6Pool = "2001:2::/64"
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
	DockerIPv4Pool = "198.18.0.1/16"

	DefaultNetDir = "/etc/cni/net.d"

	Proc = "/proc"

	CniNetName = "cni-net-dir-kubevpn"

	// env name
	EnvInboundPodTunIPv4 = "TunIPv4"
	EnvInboundPodTunIPv6 = "TunIPv6"
	EnvPodName           = "POD_NAME"
	EnvPodNamespace      = "POD_NAMESPACE"

	// header name
	HeaderIPv4 = "IPv4"
	HeaderIPv6 = "IPv6"

	KUBECONFIG = "kubeconfig"

	// labels
	ManageBy = konfig.ManagedbyLabelKey

	// pprof port
	PProfPort     = 32345
	SudoPProfPort = 33345
	PProfDir      = "pprof"

	EnvSSHJump = "SSH_JUMP_BY_KUBEVPN"

	// hosts entry key word
	HostsKeyWord = "# Add by KubeVPN"
)

var (
	// Image inject --ldflags -X
	Image     = "ghcr.io/kubenetworks/kubevpn:latest"
	Version   = "latest"
	GitCommit = ""

	// GitHubOAuthToken --ldflags -X
	GitHubOAuthToken = ""

	OriginImage = "ghcr.io/kubenetworks/kubevpn:" + Version
)

var (
	CIDR      *net.IPNet
	CIDR6     *net.IPNet
	RouterIP  net.IP
	RouterIP6 net.IP

	// for creating docker network
	DockerCIDR     *net.IPNet
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

var Debug bool

var (
	SmallBufferSize  = 8 * 1024  // 8KB small buffer
	MediumBufferSize = 32 * 1024 // 32KB medium buffer
	LargeBufferSize  = 64 * 1024 // 64KB large buffer
)

var (
	KeepAliveTime    = 60 * time.Second
	DialTimeout      = 15 * time.Second
	HandshakeTimeout = 5 * time.Second
	ConnectTimeout   = 5 * time.Second
	ReadTimeout      = 10 * time.Second
	WriteTimeout     = 10 * time.Second
)

var (
	// DefaultMTU
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

	mtu: 1417
	*/
	DefaultMTU = 1500 - max(20, 40) - 20 - 5 - 2 - 16
)

var (
	SPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, SmallBufferSize)
		},
	}
	MPool = sync.Pool{
		New: func() any {
			return make([]byte, MediumBufferSize)
		},
	}
	LPool = sync.Pool{
		New: func() any {
			return make([]byte, LargeBufferSize)
		},
	}
)

type Engine string

const (
	EngineGvisor Engine = "gvisor"
	EngineSystem Engine = "system"
)

const Slogan = "Now you can access resources in the kubernetes cluster !"
