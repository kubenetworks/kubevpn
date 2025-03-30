package config

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sigs.k8s.io/kustomize/api/konfig"
)

const (
	// configmap name
	ConfigMapPodTrafficManager = "kubevpn-traffic-manager"

	// const namespace
	KubevpnNamespace = "kubevpn-system"

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

	// container name
	ContainerSidecarEnvoyProxy   = "envoy-proxy"
	ContainerSidecarControlPlane = "control-plane"
	ContainerSidecarVPN          = "vpn"
	ContainerSidecarSyncthing    = "syncthing"

	VolumeEnvoyConfig = "envoy-config"
	VolumeSyncthing   = "syncthing"

	// innerIPv4Pool is used as tun ip
	// 198.19.0.0/16 network  is part of the 198.18.0.0/15 (reserved for benchmarking).
	// https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml
	innerIPv4Pool = "198.19.0.100/16"
	// 原因：在docker环境中，设置docker的 gateway 和 subnet，不能 inner 的冲突，也不能和 docker的 172.17 冲突
	// 不然的话，请求会不通的
	// 解决的问题：在 k8s 中的  名叫 kubernetes 的 service ip 为
	// ➜  ~ kubectl get service kubernetes
	//NAME         TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)   AGE
	//kubernetes   ClusterIP   172.17.0.1   <none>        443/TCP   190d
	//
	// ➜  ~ docker network inspect bridge | jq '.[0].IPAM.Config'
	//[
	//  {
	//    "Subnet": "172.17.0.0/16",
	//    "Gateway": "172.17.0.1"
	//  }
	//]
	// 如果不创建 network，那么是无法请求到 这个 kubernetes 的 service 的
	dockerInnerIPv4Pool = "198.18.0.100/16"

	// 2001:2::/64 network is part of the 2001:2::/48 (reserved for benchmarking)
	// https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
	innerIPv6Pool = "2001:2::9999/64"

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

	GHCR_IMAGE_REGISTRY   = "ghcr.io"
	DOCKER_IMAGE_REGISTRY = "docker.io"
)

var (
	// Image inject --ldflags -X
	Image     = "ghcr.io/kubenetworks/kubevpn:latest"
	Version   = "latest"
	GitCommit = ""

	// GitHubOAuthToken --ldflags -X
	GitHubOAuthToken = ""

	OriginImage = "ghcr.io/kubenetworks/kubevpn:" + Version

	DaemonPath string
	HomePath   string
	PprofPath  string
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
	RouterIP, CIDR, err = net.ParseCIDR(innerIPv4Pool)
	if err != nil {
		panic(err)
	}
	RouterIP6, CIDR6, err = net.ParseCIDR(innerIPv6Pool)
	if err != nil {
		panic(err)
	}
	DockerRouterIP, DockerCIDR, err = net.ParseCIDR(dockerInnerIPv4Pool)
	if err != nil {
		panic(err)
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	DaemonPath = filepath.Join(dir, HOME, Daemon)
	HomePath = filepath.Join(dir, HOME)
	PprofPath = filepath.Join(dir, HOME, Daemon, PProfDir)
}

var Debug bool

var (
	SmallBufferSize  = 8 * 1024  // 8KB small buffer
	MediumBufferSize = 32 * 1024 // 32KB medium buffer
	LargeBufferSize  = 64 * 1024 // 64KB large buffer
)

var (
	KeepAliveTime    = 180 * time.Second
	DialTimeout      = 15 * time.Second
	HandshakeTimeout = 5 * time.Second
	ConnectTimeout   = 5 * time.Second
	ReadTimeout      = 10 * time.Second
	WriteTimeout     = 10 * time.Second
)

var (
	//	network layer ip needs 20 bytes
	//	transport layer UDP header needs 8 bytes
	//	UDP over TCP header needs 22 bytes
	DefaultMTU = 1500 - 20 - 8 - 21
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
