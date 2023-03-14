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

	// config map keys
	KeyDHCP             = "DHCP"
	KeyEnvoy            = "ENVOY_CONFIG"
	KeyClusterIPv4POOLS = "IPv4_POOLS"
	KeyRefCount         = "REF_COUNT"

	// secret keys
	// TLSCertKey is the key for tls certificates in a TLS secret.
	TLSCertKey = "tls_crt"
	// TLSPrivateKeyKey is the key for the private key field in a TLS secret.
	TLSPrivateKeyKey = "tls_key"

	// container name
	ContainerSidecarEnvoyProxy   = "envoy-proxy"
	ContainerSidecarControlPlane = "control-plane"
	ContainerSidecarVPN          = "vpn"

	VolumeEnvoyConfig = "envoy-config"

	innerIPv4Pool = "223.254.0.100/16"

	DefaultNetDir = "/etc/cni/net.d"

	Proc = "/proc"

	CniNetName = "cni-net-dir-kubevpn"

	// env name
	EnvTunNameOrLUID   = "TunNameOrLUID"
	EnvInboundPodTunIP = "InboundPodTunIP"
	EnvPodName         = "POD_NAME"
	EnvPodNamespace    = "POD_NAMESPACE"

	// header name
	HeaderPodName      = "POD_NAME"
	HeaderPodNamespace = "POD_NAMESPACE"
	HeaderIP           = "IP"

	// api
	APIRentIP    = "/rent/ip"
	APIReleaseIP = "/release/ip"

	KUBECONFIG = "kubeconfig"

	// labels
	ManageBy = konfig.ManagedbyLabelKey
)

var (
	// Image inject --ldflags -X
	Image = "docker.io/naison/kubevpn:latest"
)

var CIDR *net.IPNet

var RouterIP net.IP

func init() {
	RouterIP, CIDR, _ = net.ParseCIDR(innerIPv4Pool)
}

var Debug bool

var (
	SmallBufferSize  = (1 << 13) - 1 // 8KB small buffer
	MediumBufferSize = (1 << 15) - 1 // 32KB medium buffer
	LargeBufferSize  = (1 << 16) - 1 // 64KB large buffer
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
	LPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, LargeBufferSize)
		},
	}
)
